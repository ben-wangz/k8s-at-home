package apimock

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (server *Server) takeConfigMapGetFault() getFault {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.faults.FailConfigMapGetStatus != 0 {
		status := server.faults.FailConfigMapGetStatus
		server.faults.FailConfigMapGetStatus = 0
		return getFault{kind: "status", status: status}
	}
	if server.faults.DropConfigMapGet {
		server.faults.DropConfigMapGet = false
		return getFault{kind: "drop"}
	}
	if server.faults.DelayConfigMapGet > 0 {
		delay := server.faults.DelayConfigMapGet
		server.faults.DelayConfigMapGet = 0
		return getFault{kind: "delay", delay: delay}
	}
	return getFault{}
}

func (server *Server) waitForBarrier(name string) bool {
	server.mu.Lock()
	if server.barrierName == "" || name != server.barrierName {
		server.mu.Unlock()
		return false
	}
	server.barrierCount++
	server.stats.BarrierRequests = server.barrierCount
	if server.barrierCount >= 2 {
		select {
		case <-server.barrierDone:
		default:
			close(server.barrierDone)
		}
	}
	done := server.barrierDone
	server.mu.Unlock()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
	return false
}

func (server *Server) getObject(writer http.ResponseWriter, resource, namespace, name string) {
	server.mu.Lock()
	object, exists := server.objects[objectKey(resource, namespace, name)]
	copy, cloneErr := clone(object)
	server.mu.Unlock()
	if !exists {
		server.writeStatus(writer, http.StatusNotFound, "NotFound")
		return
	}
	if cloneErr != nil {
		server.writeStatus(writer, http.StatusInternalServerError, "InternalError")
		return
	}
	server.writeJSON(writer, http.StatusOK, copy)
}

func (server *Server) createObject(writer http.ResponseWriter, request *http.Request, resource, namespace string) {
	var object map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20)).Decode(&object); err != nil {
		server.writeStatus(writer, http.StatusBadRequest, "BadRequest")
		return
	}
	metadata := asMap(object["metadata"])
	name := stringValue(metadata["name"])
	if name == "" || stringValue(metadata["namespace"]) != namespace || (resource == "secrets" && object["kind"] != "Secret") || (resource == "configmaps" && object["kind"] != "ConfigMap") {
		server.writeStatus(writer, http.StatusBadRequest, "BadRequest")
		return
	}
	server.mu.Lock()
	if resource == "secrets" {
		server.stats.CreateSecrets++
		if server.faults.FailSecretCreateStatus != 0 {
			status := server.faults.FailSecretCreateStatus
			server.faults.FailSecretCreateStatus = 0
			server.mu.Unlock()
			server.writeStatus(writer, status, statusReason(status))
			return
		}
	}
	if resource == "configmaps" {
		server.stats.CreateConfigMaps++
	}
	key := objectKey(resource, namespace, name)
	if _, exists := server.objects[key]; exists {
		server.mu.Unlock()
		server.writeStatus(writer, http.StatusConflict, "AlreadyExists")
		return
	}
	server.uidSequence++
	metadata["uid"] = fmt.Sprintf("test-uid-%04d", server.uidSequence)
	metadata["resourceVersion"] = "1"
	object["metadata"] = metadata
	server.objects[key] = object
	dropResponse := false
	if resource == "secrets" && server.faults.DropSecretCreateResponse {
		server.faults.DropSecretCreateResponse = false
		dropResponse = true
	}
	if resource == "configmaps" && server.faults.DropRecordCreateResponse {
		server.faults.DropRecordCreateResponse = false
		dropResponse = true
	}
	server.mu.Unlock()
	if dropResponse {
		server.dropConnection(writer)
		return
	}
	server.writeJSON(writer, http.StatusCreated, object)
}

func (server *Server) updateConfigMap(writer http.ResponseWriter, request *http.Request, namespace, name string) {
	var object map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20)).Decode(&object); err != nil {
		server.writeStatus(writer, http.StatusBadRequest, "BadRequest")
		return
	}
	server.mu.Lock()
	server.stats.UpdateConfigMaps++
	if server.faults.ConflictConfigMapUpdates > 0 {
		server.faults.ConflictConfigMapUpdates--
		server.mu.Unlock()
		server.writeStatus(writer, http.StatusConflict, "Conflict")
		return
	}
	if server.faults.FailConfigMapUpdateStatus != 0 {
		status := server.faults.FailConfigMapUpdateStatus
		server.faults.FailConfigMapUpdateStatus = 0
		server.mu.Unlock()
		server.writeStatus(writer, status, statusReason(status))
		return
	}
	key := objectKey("configmaps", namespace, name)
	current, exists := server.objects[key]
	if !exists {
		server.mu.Unlock()
		server.writeStatus(writer, http.StatusNotFound, "NotFound")
		return
	}
	metadata := asMap(object["metadata"])
	currentMetadata := asMap(current["metadata"])
	if stringValue(metadata["resourceVersion"]) != stringValue(currentMetadata["resourceVersion"]) {
		server.mu.Unlock()
		server.writeStatus(writer, http.StatusConflict, "Conflict")
		return
	}
	nextVersion := server.uidSequence + 1
	server.uidSequence = nextVersion
	metadata["uid"] = currentMetadata["uid"]
	metadata["resourceVersion"] = strconv.Itoa(nextVersion)
	object["metadata"] = metadata
	server.objects[key] = object
	server.mu.Unlock()
	server.writeJSON(writer, http.StatusOK, object)
}

func (server *Server) writeStatus(writer http.ResponseWriter, status int, reason string) {
	server.writeJSON(writer, status, map[string]any{
		"apiVersion": "v1", "kind": "Status", "status": "Failure", "reason": reason,
		"message": reason, "code": status,
	})
}

func (server *Server) writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func (server *Server) dropConnection(writer http.ResponseWriter) {
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		return
	}
	connection, _, err := hijacker.Hijack()
	if err == nil {
		_ = connection.Close()
	}
}

func parsePath(path string) (resource, namespace, name string, collection, ok bool) {
	parts := strings.Split(path, "/")
	if len(parts) != 6 && len(parts) != 7 {
		return "", "", "", false, false
	}
	if parts[1] != "api" || parts[2] != "v1" || parts[3] != "namespaces" || parts[5] != "secrets" && parts[5] != "configmaps" {
		return "", "", "", false, false
	}
	resource, namespace = parts[5], parts[4]
	if len(parts) == 6 {
		return resource, namespace, "", true, true
	}
	return resource, namespace, parts[6], false, parts[6] != ""
}

func objectKey(resource, namespace, name string) string {
	return resource + "\x00" + namespace + "\x00" + name
}

func clone(value map[string]any) (map[string]any, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func asMap(value any) map[string]any {
	result, _ := value.(map[string]any)
	if result == nil {
		result = make(map[string]any)
	}
	return result
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}
