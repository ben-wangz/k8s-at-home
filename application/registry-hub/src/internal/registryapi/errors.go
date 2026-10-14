package registryapi

import (
	"encoding/json"
	"net/http"
)

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail,omitempty"`
}

type ErrorEnvelope struct {
	Errors []Error `json:"errors"`
}

func WriteError(writer http.ResponseWriter, status int, code, message string) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(ErrorEnvelope{Errors: []Error{{Code: code, Message: message}}})
}

func WriteErrorWithDetail(writer http.ResponseWriter, status int, code, message string, detail any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(ErrorEnvelope{Errors: []Error{{Code: code, Message: message, Detail: detail}}})
}
