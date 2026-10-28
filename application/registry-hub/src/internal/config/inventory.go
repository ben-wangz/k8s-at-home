package config

import (
	"errors"
	"time"
)

type Inventory struct {
	RefreshInterval   time.Duration
	StaleAfter        time.Duration
	SnapshotRetention time.Duration
	BatchSize         int
	BatchTimeout      time.Duration
}

func DefaultInventory() Inventory {
	return Inventory{RefreshInterval: 30 * time.Second, StaleAfter: 2 * time.Minute, SnapshotRetention: time.Hour, BatchSize: 100, BatchTimeout: time.Minute}
}

func (c Inventory) validateOptional() error {
	if c == (Inventory{}) {
		return nil
	}
	return c.Validate()
}

func (c Inventory) Validate() error {
	if c.RefreshInterval < 5*time.Second || c.RefreshInterval > time.Hour || c.StaleAfter < c.RefreshInterval || c.StaleAfter > 24*time.Hour || c.SnapshotRetention < 15*time.Minute || c.SnapshotRetention > 7*24*time.Hour || c.SnapshotRetention <= c.StaleAfter || c.BatchSize < 1 || c.BatchSize > 1000 || c.BatchTimeout < time.Second || c.BatchTimeout > 10*time.Minute {
		return errors.New("inventory intervals, retention, batch size or timeout are outside supported bounds")
	}
	return nil
}
