package app

import (
	"context"
	"errors"
	"net/http"
	"registry-hub/internal/download"
	"time"
)

func pingWithRetry(ctx context.Context, ping func(context.Context) error, interval time.Duration) error {
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return errors.Join(errors.New("Distribution did not become ready before startup deadline"), lastErr, err)
			}
			return err
		}
		pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		lastErr = ping(pingCtx)
		cancel()
		if lastErr == nil {
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(errors.New("Distribution did not become ready before startup deadline"), lastErr, ctx.Err())
		case <-timer.C:
		}
	}
}

func (s *Service) ClientHandler() http.Handler {
	return http.HandlerFunc(s.handleClient)
}

func (s *Service) downloadManager() *download.Manager {
	s.downloadOnce.Do(func() {
		s.downloader = download.New(s.store, s.cfg.Limits.DownloadMaxBytes)
	})
	return s.downloader
}

func (s *Service) SetReady(ready bool) { s.ready.Store(ready) }

func (s *Service) refreshReadiness() {
	if s.gate == nil {
		s.ready.Store(false)
		return
	}
	s.setReadinessForGate(s.gate.State())
}

func (s *Service) watchReadiness(ctx context.Context) {
	for {
		changed, state := s.gate.readinessSnapshot()
		s.setReadinessForGate(state)
		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
	}
}

func (s *Service) setReadinessForGate(gate gateRecord) {
	if s.ctx != nil && s.ctx.Err() != nil {
		s.ready.Store(false)
		return
	}
	s.ready.Store(s.storageHealthy.Load() && s.registryHealthy.Load() && gate.State == "open" && !gate.RecoveryBarrier && !gate.Exclusive)
}

// BeginShutdown stops background admission while the HTTP servers drain active requests.
func (s *Service) BeginShutdown() {
	s.ready.Store(false)
	s.cancel()
}
