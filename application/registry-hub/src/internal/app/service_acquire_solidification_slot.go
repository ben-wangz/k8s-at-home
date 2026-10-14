package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"registry-hub/internal/storage"
	"strings"
	"sync"
)

func (s *Service) acquireSolidificationSlot(ctx context.Context, repair bool) (func(), bool) {
	if ctx.Err() != nil {
		return nil, false
	}
	s.solidificationMu.Lock()
	active := len(s.solidificationSlots)
	limit := cap(s.solidificationSlots)
	if s.repairPending && limit > 1 {
		if repair && s.referrerRepairs >= limit-1 || !repair && s.newSolidifications >= 1 {
			s.solidificationMu.Unlock()
			return nil, false
		}
	}
	if active >= limit {
		s.solidificationMu.Unlock()
		return nil, false
	}
	s.solidificationSlots <- struct{}{}
	if repair {
		s.referrerRepairs++
	} else {
		s.newSolidifications++
	}
	s.solidificationMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.solidificationMu.Lock()
			<-s.solidificationSlots
			if repair {
				s.referrerRepairs--
			} else {
				s.newSolidifications--
			}
			s.solidificationMu.Unlock()
		})
	}, true
}

func (s *Service) upstreamSlot(authority string) chan struct{} {
	hash := 0
	for _, value := range strings.ToLower(authority) {
		hash = (hash*31 + int(value)) & 255
	}
	return s.upstreamPerKey[hash]
}

func (s *Service) sourceAddress(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err == nil {
		return host
	}
	return request.RemoteAddr
}

func (s *Service) acquireClient(request *http.Request, authenticated bool) (func(), bool) {
	releaseClient, ok := s.acquireSlot(request.Context(), s.clientSlots)
	if !ok {
		return nil, false
	}
	if authenticated {
		return releaseClient, true
	}
	releaseAnonymous, ok := s.acquireSlot(request.Context(), s.noAuthSlots)
	if !ok {
		releaseClient()
		return nil, false
	}
	return func() {
		releaseAnonymous()
		releaseClient()
	}, true
}

func (s *Service) remoteRequestAllowed(ctx context.Context, authority string) (func(), bool) {
	globalRelease, ok := s.acquireSlot(ctx, s.upstreamSlots)
	if !ok {
		return nil, false
	}
	authorityRelease, ok := s.acquireSlot(ctx, s.upstreamSlot(authority))
	if !ok {
		globalRelease()
		return nil, false
	}
	return func() {
		authorityRelease()
		globalRelease()
	}, true
}

func storageError(err error) bool {
	return err != nil && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrConditionFailed)
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
