package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	host := os.Getenv("CLASH_FIXTURE_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	server := &http.Server{Addr: ":8080", Handler: handler(host), ReadHeaderTimeout: 10 * time.Second}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	go func() {
		<-ctx.Done()
		shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = server.Shutdown(shutdown)
	}()
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func handler(host string) http.Handler {
	transport := &http.Transport{Proxy: nil, ResponseHeaderTimeout: 10 * time.Second}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			proxyConnect(w, r, host)
			return
		}
		if r.URL.IsAbs() {
			// This fixture proxies only its own traffic destination.
			if r.URL.Hostname() != host || r.URL.Port() != "8080" {
				http.Error(w, "fixture destination rejected", 403)
				return
			}
			request := r.Clone(r.Context())
			request.RequestURI = ""
			response, err := transport.RoundTrip(request)
			if err != nil {
				http.Error(w, "fixture upstream unavailable", 502)
				return
			}
			defer response.Body.Close()
			w.WriteHeader(response.StatusCode)
			buffer := make([]byte, 4096)
			for {
				n, err := response.Body.Read(buffer)
				if n > 0 {
					_, _ = w.Write(buffer[:n])
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
				}
				if err != nil {
					return
				}
			}
		}
		switch r.URL.Path {
		case "/subscription":
			w.Header().Set("Content-Type", "text/yaml")
			fmt.Fprintf(w, "proxies:\n  - name: Node A\n    type: http\n    server: %s\n    port: 8080\n  - name: Node / B\n    type: http\n    server: %s\n    port: 8080\n", host, host)
		case "/stream":
			w.Header().Set("Content-Type", "application/octet-stream")
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-r.Context().Done():
					return
				case <-ticker.C:
					if _, err := io.WriteString(w, strings.Repeat("clash-console-traffic ", 512)); err != nil {
						return
					}
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
				}
			}
		case "/health":
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
	})
}

func proxyConnect(w http.ResponseWriter, r *http.Request, host string) {
	requestedHost, port, err := net.SplitHostPort(r.Host)
	if err != nil || requestedHost != host || port != "8080" {
		http.Error(w, "fixture destination rejected", http.StatusForbidden)
		return
	}
	upstream, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(r.Context(), "tcp", r.Host)
	if err != nil {
		http.Error(w, "fixture upstream unavailable", http.StatusBadGateway)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "CONNECT is unavailable", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	defer client.Close()
	defer upstream.Close()
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := buffered.Flush(); err != nil {
		return
	}
	go func() {
		_, _ = io.Copy(upstream, io.MultiReader(buffered.Reader, client))
		_ = upstream.Close()
	}()
	_, _ = io.Copy(client, upstream)
}
