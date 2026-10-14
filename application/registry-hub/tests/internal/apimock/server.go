package apimock

import (
	"crypto/tls"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const tokenValue = "registry-hub-tests-local-api-token"

type Faults struct {
	DropRecordCreateResponse  bool
	DropSecretCreateResponse  bool
	ConflictConfigMapUpdates  int
	FailSecretCreateStatus    int
	FailConfigMapUpdateStatus int
	FailConfigMapGetStatus    int
	DropConfigMapGet          bool
	DelayConfigMapGet         time.Duration
}

type Stats struct {
	CreateSecrets    int `json:"createSecretRequests"`
	CreateConfigMaps int `json:"createConfigMapRequests"`
	UpdateConfigMaps int `json:"updateConfigMapRequests"`
	CommittedSecrets int `json:"committedSecrets"`
	BarrierRequests  int `json:"concurrentBarrierRequests"`
}

type Server struct {
	mu           sync.Mutex
	objects      map[string]map[string]any
	faults       Faults
	stats        Stats
	uidSequence  int
	barrierName  string
	barrierCount int
	barrierDone  chan struct{}
	listener     net.Listener
	httpServer   *http.Server
	serveDone    chan struct{}
	Port         int
	ServiceDir   string
}

func New(directory, barrierName string) (*Server, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(directory, 0700); err != nil {
		return nil, err
	}
	certificate, caPEM, err := serverCertificate()
	if err != nil {
		return nil, err
	}
	serviceDir := filepath.Join(directory, "service-account")
	if err := os.MkdirAll(serviceDir, 0700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(serviceDir, "ca.crt"), caPEM, 0600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(serviceDir, "token"), []byte(tokenValue), 0600); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	_, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		listener.Close()
		return nil, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		listener.Close()
		return nil, err
	}
	api := &Server{
		objects: make(map[string]map[string]any), barrierName: barrierName,
		barrierDone: make(chan struct{}), listener: listener, Port: port,
		ServiceDir: serviceDir, serveDone: make(chan struct{}),
	}
	api.httpServer = &http.Server{Handler: http.HandlerFunc(api.handle)}
	tlsListener := tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
	go func() {
		defer close(api.serveDone)
		_ = api.httpServer.Serve(tlsListener)
	}()
	return api, nil
}

func (server *Server) SetFaults(faults Faults) {
	server.mu.Lock()
	server.faults = faults
	server.mu.Unlock()
}

func (server *Server) Close() {
	_ = server.httpServer.Close()
	<-server.serveDone
}

func (server *Server) Stats() Stats {
	server.mu.Lock()
	defer server.mu.Unlock()
	stats := server.stats
	stats.CommittedSecrets = 0
	for key := range server.objects {
		if strings.HasPrefix(key, "secrets\x00") {
			stats.CommittedSecrets++
		}
	}
	return stats
}

func (server *Server) Objects() int {
	server.mu.Lock()
	defer server.mu.Unlock()
	return len(server.objects)
}

func (server *Server) Object(resource, name string) (map[string]any, bool) {
	server.mu.Lock()
	defer server.mu.Unlock()
	value, exists := server.objects[objectKey(resource, "develop", name)]
	if !exists {
		return nil, false
	}
	copy, err := clone(value)
	return copy, err == nil
}

func (server *Server) DeleteObject(resource, name string) {
	server.mu.Lock()
	delete(server.objects, objectKey(resource, "develop", name))
	server.mu.Unlock()
}

func (server *Server) SetSecretData(name, key, value string) {
	server.mu.Lock()
	defer server.mu.Unlock()
	secret, exists := server.objects[objectKey("secrets", "develop", name)]
	if !exists {
		return
	}
	data := asMap(secret["data"])
	data[key] = value
	secret["data"] = data
}

func (server *Server) SetSecretUID(name, uid string) {
	server.mu.Lock()
	defer server.mu.Unlock()
	secret, exists := server.objects[objectKey("secrets", "develop", name)]
	if !exists {
		return
	}
	metadata := asMap(secret["metadata"])
	metadata["uid"] = uid
}

func (server *Server) SetRecordScope(name, scope string) {
	server.mu.Lock()
	defer server.mu.Unlock()
	config, exists := server.objects[objectKey("configmaps", "develop", name)]
	if !exists {
		return
	}
	metadata := asMap(config["metadata"])
	annotations := asMap(metadata["annotations"])
	annotations["registry-hub.io/internal-scope"] = scope
}

func (server *Server) handle(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") != "Bearer "+tokenValue {
		server.writeStatus(writer, http.StatusUnauthorized, "Unauthorized")
		return
	}
	resource, namespace, name, collection, ok := parsePath(request.URL.Path)
	if !ok || namespace != "develop" {
		server.writeStatus(writer, http.StatusNotFound, "NotFound")
		return
	}
	if request.Method == http.MethodGet && resource == "configmaps" && server.waitForBarrier(name) {
		return
	}
	if request.Method == http.MethodGet && resource == "configmaps" {
		fault := server.takeConfigMapGetFault()
		switch fault.kind {
		case "status":
			server.writeStatus(writer, fault.status, statusReason(fault.status))
			return
		case "drop":
			server.dropConnection(writer)
			return
		case "delay":
			timer := time.NewTimer(fault.delay)
			defer timer.Stop()
			select {
			case <-request.Context().Done():
				return
			case <-timer.C:
			}
		}
	}
	switch {
	case request.Method == http.MethodGet && !collection:
		server.getObject(writer, resource, namespace, name)
	case request.Method == http.MethodPost && collection:
		server.createObject(writer, request, resource, namespace)
	case request.Method == http.MethodPut && !collection && resource == "configmaps":
		server.updateConfigMap(writer, request, namespace, name)
	default:
		server.writeStatus(writer, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

type getFault struct {
	kind   string
	status int
	delay  time.Duration
}
