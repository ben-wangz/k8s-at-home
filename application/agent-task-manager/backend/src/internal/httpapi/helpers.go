package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/store/bunstore"
)

func splitPath(path string) []string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 1 && parts[0] == "" {
		return nil
	}
	return parts
}

func parseBool(value string) bool {
	parsed, _ := strconv.ParseBool(value)
	return parsed
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return false
	}
	return true
}

func writeStoreResult(w http.ResponseWriter, status int, payload any, err error) {
	if err == nil {
		writeJSON(w, status, payload)
		return
	}
	if errors.Is(err, bunstore.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
}

func writeDeleteResult(w http.ResponseWriter, err error) {
	if err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeStoreResult(w, http.StatusNoContent, nil, err)
}

func writeMethodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}

func writeNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "not_found", "not found")
}

func writeAuthError(w http.ResponseWriter, err error) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="agent-task-manager"`)
	if errors.Is(err, errMissingBearerToken) {
		writeError(w, http.StatusUnauthorized, "missing_bearer_token", err.Error())
		return
	}
	if errors.Is(err, errInvalidAPIKey) {
		writeError(w, http.StatusUnauthorized, "invalid_api_key", err.Error())
		return
	}
	writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "error": message})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
