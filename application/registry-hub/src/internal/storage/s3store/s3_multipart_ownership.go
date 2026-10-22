package s3store

import (
	"context"
)

func (s *Store) beginMultipart(ctx context.Context, key string) (func(), error) {
	for {
		s.multipartMu.Lock()
		if !s.multipartCleaning[key] {
			if s.multipartActive == nil {
				s.multipartActive = make(map[string]int)
			}
			s.multipartActive[key]++
			s.multipartMu.Unlock()
			return func() { s.releaseMultipart(key, false) }, nil
		}
		changed := s.multipartChanged
		if changed == nil {
			changed = make(chan struct{})
			s.multipartChanged = changed
		}
		s.multipartMu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (s *Store) claimMultipartCleanup(key string) func() {
	s.multipartMu.Lock()
	defer s.multipartMu.Unlock()
	if s.multipartActive[key] > 0 || s.multipartCleaning[key] {
		return nil
	}
	if s.multipartCleaning == nil {
		s.multipartCleaning = make(map[string]bool)
	}
	s.multipartCleaning[key] = true
	if s.multipartChanged == nil {
		s.multipartChanged = make(chan struct{})
	}
	return func() { s.releaseMultipart(key, true) }
}

func (s *Store) releaseMultipart(key string, cleaning bool) {
	s.multipartMu.Lock()
	if cleaning {
		delete(s.multipartCleaning, key)
	} else if s.multipartActive[key] <= 1 {
		delete(s.multipartActive, key)
	} else {
		s.multipartActive[key]--
	}
	if s.multipartChanged != nil {
		close(s.multipartChanged)
		s.multipartChanged = make(chan struct{})
	}
	s.multipartMu.Unlock()
}
