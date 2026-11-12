package upstreamauthtransport

import (
	"context"
	"net"
	"testing"
)

func TestBackendClientPortProbeChecksTheRegistryListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	clientURL := "http://" + listener.Addr().String()
	if !backendClientPortOpen(context.Background(), clientURL) {
		t.Fatal("open registry client port was not detected")
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if backendClientPortOpen(context.Background(), clientURL) {
		t.Fatal("closed registry client port was detected as ready")
	}
}
