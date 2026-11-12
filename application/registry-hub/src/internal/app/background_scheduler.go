package app

import (
	"context"
	"sync"
	"time"
)

type backgroundWaiter struct {
	ready    chan struct{}
	at       time.Time
	kind     string
	urgent   bool
	admitted bool
}

type backgroundScheduler struct {
	mu                            sync.Mutex
	capacity, active, urgentBurst int
	waiters                       []*backgroundWaiter
}

func (s *backgroundScheduler) acquire(ctx context.Context, kind string, urgent bool, hint <-chan struct{}) (func(), bool) {
	w := &backgroundWaiter{ready: make(chan struct{}), at: time.Now(), kind: kind, urgent: urgent}
	s.mu.Lock()
	if ctx.Err() != nil {
		s.mu.Unlock()
		return nil, false
	}
	if len(hint) > 0 {
		w.urgent = true
	}
	s.waiters = append(s.waiters, w)
	s.dispatch()
	s.mu.Unlock()
	select {
	case <-w.ready:
	case <-ctx.Done():
	}
	s.mu.Lock()
	if ctx.Err() != nil {
		if w.admitted {
			s.active--
		} else {
			for i, pending := range s.waiters {
				if pending == w {
					s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
					break
				}
			}
		}
		s.dispatch()
		s.mu.Unlock()
		return nil, false
	}
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.active--
			s.dispatch()
		})
	}, true
}

func (s *backgroundScheduler) promote(kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, waiter := range s.waiters {
		if waiter.kind == kind {
			waiter.urgent = true
		}
	}
	s.dispatch()
}

func (s *backgroundScheduler) dispatch() {
	for s.active < s.capacity && len(s.waiters) > 0 {
		urgent, maintenance := -1, -1
		for i, w := range s.waiters {
			if w.urgent && urgent < 0 {
				urgent = i
			}
			if !w.urgent && maintenance < 0 {
				maintenance = i
			}
		}
		next := urgent
		if maintenance >= 0 && (urgent < 0 || s.urgentBurst >= 3 || s.urgentBurst > 0 && time.Since(s.waiters[maintenance].at) >= 2*time.Second) {
			next = maintenance
		}
		w := s.waiters[next]
		s.waiters = append(s.waiters[:next], s.waiters[next+1:]...)
		if w.urgent {
			s.urgentBurst++
		} else {
			s.urgentBurst = 0
		}
		s.active++
		w.admitted = true
		close(w.ready)
	}
}

type backgroundIdleKey struct{}

func urgentBackgroundKind(ctx context.Context, kind string) bool {
	if idle, _ := ctx.Value(backgroundIdleKey{}).(bool); idle {
		return false
	}
	switch kind {
	case "notify", "wake_root", "quota_wake", "terminal_cleanup", "download_cleanup", "execution", "inventory", "activation_events":
		return true
	default:
		return false
	}
}
