package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"time"
)

type diagnosticQuota struct {
	Reservations map[string]struct {
		Identity string `json:"identity"`
		Digest   string `json:"digest"`
		Session  string `json:"session"`
		Bytes    int64  `json:"bytes"`
	} `json:"reservations"`
}

func (s *Service) readDiagnosticQuota(ctx context.Context) (diagnosticQuota, error) {
	quota, _, err := inventory.Read[diagnosticQuota](ctx, s.store, "downloads/v1/quota.json")
	if errors.Is(err, storage.ErrNotFound) {
		return diagnosticQuota{}, nil
	}
	if err != nil {
		return diagnosticQuota{}, err
	}
	if len(quota.Reservations) > 128 {
		return diagnosticQuota{}, storage.ErrCorruptObject
	}
	for _, reservation := range quota.Reservations {
		if reservation.Bytes < 0 {
			return diagnosticQuota{}, storage.ErrCorruptObject
		}
	}
	return quota, nil
}

func (s *Service) observeTransferReservation(ctx context.Context, view *transferView) {
	quota, err := s.readDiagnosticQuota(ctx)
	if err != nil {
		return
	}
	bytes := int64(0)
	for _, reservation := range quota.Reservations {
		if reservation.Bytes < 0 {
			return
		}
		if reservation.Session == view.SessionID {
			bytes += reservation.Bytes
		}
	}
	view.ReservedBytes = &bytes
	view.ReservationObservedAt = time.Now().UTC()
	view.QuotaReleaseState = "released"
	if bytes > 0 {
		view.QuotaReleaseState = "reserved"
	}
}
