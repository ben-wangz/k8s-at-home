package app

import (
	"registry-hub/internal/workqueue"
	"time"
)

func (s *Service) backgroundQueue() workqueue.Queue {
	return workqueue.Queue{Store: s.store, OnRunnable: s.signalBackgroundQueue, BuildFinalSummary: buildBackgroundFinalSummary}
}

func (s *Service) signalBackgroundQueue(kind string) {
	select {
	case s.backgroundReady[kind] <- struct{}{}:
	default:
	}
	if s.backgroundScheduler != nil {
		s.backgroundScheduler.promote(kind)
	}
}

func nextQueuePoll(idle time.Duration, empty bool) time.Duration {
	if !empty {
		return time.Second
	}
	return min(max(time.Second, idle*2), 5*time.Second)
}
