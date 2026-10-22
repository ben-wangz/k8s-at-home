package app

import (
	"context"
	"time"
)

func (s *Service) abortInternalUpload(location, internalRepository string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.registry.CancelUpload(ctx, location, internalRepository); err != nil {
		s.logger.Warn("internal upload cleanup was not confirmed", "error_class", "distribution")
	}
}
