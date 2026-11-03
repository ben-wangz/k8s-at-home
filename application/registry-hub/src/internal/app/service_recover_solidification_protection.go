package app

import (
	"context"
	"errors"
)

const legacyGrandfatheredSessionID = "f2b4944d5d74f22a6a4e357fa693e96d"

const legacyGrandfatheredScopeID = "legacy-grandfathered"

func (s *Service) recoverSolidificationProtection(ctx context.Context) error {
	for {
		err := s.recoverProtectionQuantum(ctx)
		if !errors.Is(err, errProtectionRecoveryYield) {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}
