package app

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"registry-hub/internal/registryapi"
)

func decodeAdminBody(writer http.ResponseWriter, request *http.Request, target any) error {
	defer request.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return ensureAdminEOF(decoder)
}

func ensureAdminEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("unexpected trailing management request body")
	}
	return nil
}

func onlyQueryKeys(query url.Values, keys ...string) bool {
	allowed := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		allowed[key] = struct{}{}
	}
	for key, values := range query {
		if _, ok := allowed[key]; !ok || len(values) != 1 {
			return false
		}
	}
	return true
}

func writeAdminError(writer http.ResponseWriter, status int, code, message string) {
	registryapi.WriteError(writer, status, code, message)
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
