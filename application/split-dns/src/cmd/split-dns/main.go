package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"split-dns/internal/config"
	"split-dns/internal/dnsrouter"
	"split-dns/internal/dnsserver"
)

func main() {
	configPath := flag.String("config", "/etc/split-dns/config.json", "path to the DNS configuration")
	flag.Parse()
	if err := run(*configPath); err != nil {
		slog.Error("split-dns stopped", "error", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	registry := prometheus.NewRegistry()
	metrics := dnsrouter.NewMetrics(registry)
	router, err := dnsrouter.New(cfg, metrics, logger)
	if err != nil {
		return err
	}
	defer router.Close()

	udpConn, err := net.ListenPacket("udp", net.JoinHostPort(cfg.ListenAddress, strconv.Itoa(cfg.ListenPort)))
	if err != nil {
		return fmt.Errorf("listen for UDP DNS: %w", err)
	}
	tcpListener, err := net.Listen("tcp", net.JoinHostPort(cfg.ListenAddress, strconv.Itoa(cfg.ListenPort)))
	if err != nil {
		_ = udpConn.Close()
		return fmt.Errorf("listen for TCP DNS: %w", err)
	}
	metricsListener, err := net.Listen("tcp", net.JoinHostPort(cfg.Metrics.Address, strconv.Itoa(cfg.Metrics.Port)))
	if err != nil {
		_ = udpConn.Close()
		_ = tcpListener.Close()
		return fmt.Errorf("listen for health and metrics: %w", err)
	}

	var ready atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/readyz", func(writer http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	})
	if cfg.Metrics.Enabled {
		mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	}
	healthServer := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	dnsUDP := &dns.Server{PacketConn: udpConn, Handler: router, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	tcpServerCtx, cancelTCPServer := context.WithCancel(context.Background())
	defer cancelTCPServer()

	errorsOut := make(chan error, 3)
	go func() { errorsOut <- dnsUDP.ActivateAndServe() }()
	tcpDone := make(chan struct{})
	go func() {
		defer close(tcpDone)
		errorsOut <- dnsserver.ServeTCP(tcpServerCtx, tcpListener, router)
	}()
	go func() { errorsOut <- healthServer.Serve(metricsListener) }()
	ready.Store(true)
	logger.Info("DNS listeners ready", "listenAddress", cfg.ListenAddress, "listenPort", cfg.ListenPort, "metricsEnabled", cfg.Metrics.Enabled)

	var runErr error
	signals, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case <-signals.Done():
	case err := <-errorsOut:
		if err == nil {
			runErr = errors.New("listener stopped unexpectedly")
		} else if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("listener failed", "error", err)
			runErr = err
		}
	}
	ready.Store(false)
	router.Close()
	cancelTCPServer()
	<-tcpDone
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = dnsUDP.ShutdownContext(shutdownCtx)
	if err := healthServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown health server: %w", err)
	}
	return runErr
}
