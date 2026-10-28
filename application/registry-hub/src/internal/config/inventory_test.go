package config

import (
	"testing"
	"time"
)

func TestInventoryConfigurationBounds(t *testing.T) {
	if err := DefaultInventory().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Inventory){func(c *Inventory) { c.BatchSize = 0 }, func(c *Inventory) { c.BatchSize = 1001 }, func(c *Inventory) { c.RefreshInterval = time.Second }, func(c *Inventory) { c.StaleAfter = time.Second }, func(c *Inventory) { c.SnapshotRetention = 10 * time.Minute }, func(c *Inventory) { c.BatchTimeout = 11 * time.Minute }} {
		c := DefaultInventory()
		change(&c)
		if c.Validate() == nil {
			t.Fatalf("invalid inventory config accepted: %+v", c)
		}
	}
}
