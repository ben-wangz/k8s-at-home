package app

import (
	"context"
	"registry-hub/internal/storage"
	"sync"
	"sync/atomic"
	"time"
)

type healthProbeStatus struct {
	LastProbeAt     *time.Time `json:"lastProbeAt"`
	LastSuccessAt   *time.Time `json:"lastSuccessAt"`
	Healthy         bool       `json:"healthy"`
	Stale           bool       `json:"stale"`
	ValidForSeconds int        `json:"validForSeconds"`
}

func (s *Service) runHealthProbes(ctx context.Context) {
	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		s.runHealthProbe(ctx, func(ctx context.Context) error { return storage.Probe(ctx, s.store) }, &s.storageHealthy, &s.storageProbeAt, &s.storageProbeSuccess)
	}()
	go func() {
		defer group.Done()
		s.runHealthProbe(ctx, s.registry.Ping, &s.registryHealthy, &s.registryProbeAt, &s.registryProbeSuccess)
	}()
	group.Wait()
}

func (s *Service) runHealthProbe(ctx context.Context, probe func(context.Context) error, healthy *atomic.Bool, last, success *atomic.Int64) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := probe(bounded)
		cancel()
		now := time.Now().UTC().UnixNano()
		last.Store(now)
		healthy.Store(err == nil)
		if err == nil {
			success.Store(now)
		}
		s.refreshReadiness()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func probeStatus(healthy bool, last, success int64) healthProbeStatus {
	status := healthProbeStatus{Healthy: healthy, Stale: last == 0 || time.Since(time.Unix(0, last)) > 90*time.Second, ValidForSeconds: 90}
	if last != 0 {
		at := time.Unix(0, last).UTC()
		status.LastProbeAt = &at
	}
	if success != 0 {
		at := time.Unix(0, success).UTC()
		status.LastSuccessAt = &at
	}
	return status
}
