package app

import (
	"sync"
	"time"
)

type bucket struct {
	tokens   float64
	updated  time.Time
	lastSeen time.Time
}

type requestLimiter struct {
	mu          sync.Mutex
	global      bucket
	sources     map[string]bucket
	globalRate  float64
	globalBurst float64
	noAuthRate  float64
	noAuthBurst float64
}

func newRequestLimiter(globalRate, globalBurst, noAuthRate, noAuthBurst float64) *requestLimiter {
	now := time.Now()
	return &requestLimiter{
		global: bucket{tokens: globalBurst, updated: now, lastSeen: now}, sources: make(map[string]bucket),
		globalRate: globalRate, globalBurst: globalBurst, noAuthRate: noAuthRate, noAuthBurst: noAuthBurst,
	}
}

func (l *requestLimiter) Allow(source string, noAuth bool) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	refill(&l.global, now, l.globalRate, l.globalBurst)
	var sourceBucket bucket
	if noAuth {
		var exists bool
		sourceBucket, exists = l.sources[source]
		if !exists && len(l.sources) >= 4096 {
			for key, item := range l.sources {
				if now.Sub(item.lastSeen) > time.Minute {
					delete(l.sources, key)
				}
			}
			if len(l.sources) >= 4096 {
				return false
			}
		}
		if sourceBucket.updated.IsZero() {
			sourceBucket = bucket{tokens: l.noAuthBurst, updated: now, lastSeen: now}
		}
		refill(&sourceBucket, now, l.noAuthRate, l.noAuthBurst)
		if sourceBucket.tokens < 1 {
			l.sources[source] = sourceBucket
			return false
		}
	}
	if l.global.tokens < 1 {
		if noAuth {
			l.sources[source] = sourceBucket
		}
		return false
	}
	l.global.tokens--
	if noAuth {
		sourceBucket.tokens--
		sourceBucket.lastSeen = now
		l.sources[source] = sourceBucket
	}
	return true
}

func refill(current *bucket, now time.Time, rate, burst float64) {
	if !current.updated.IsZero() {
		current.tokens += now.Sub(current.updated).Seconds() * rate
	}
	if current.tokens > burst {
		current.tokens = burst
	}
	current.updated = now
}
