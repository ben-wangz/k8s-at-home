package config

import (
	"strings"
	"testing"
)

func TestValidateEndpointHTTPPolicy(t *testing.T) {
	tests := []struct {
		name              string
		endpoint          string
		allowInsecureHTTP bool
		wantErr           string
	}{
		{name: "empty endpoint", endpoint: ""},
		{name: "https endpoint", endpoint: "https://s3.example.com"},
		{name: "http endpoint requires opt in", endpoint: "http://s3.example.com", wantErr: "allowInsecureHttp"},
		{name: "http endpoint opt in", endpoint: "http://s3.example.com", allowInsecureHTTP: true},
		{name: "unsupported scheme", endpoint: "ftp://s3.example.com", allowInsecureHTTP: true, wantErr: "must use https"},
		{name: "http endpoint with path", endpoint: "http://s3.example.com/bucket", allowInsecureHTTP: true, wantErr: "must not contain a path"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateEndpoint(tt.endpoint, tt.allowInsecureHTTP)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateEndpoint() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateEndpoint() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}
