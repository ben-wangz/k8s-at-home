package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strconv"
	"strings"
)

type inventoryDependencies struct {
	Objects  map[string]string           `json:"objects,omitempty"`
	Listings map[string]inventoryListing `json:"listings,omitempty"`
	Invalid  bool                        `json:"invalid,omitempty"`
}

type inventoryListing struct {
	Prefix      string `json:"prefix"`
	Token       string `json:"token"`
	Limit       int    `json:"limit"`
	Fingerprint string `json:"fingerprint"`
	Metadata    bool   `json:"metadata,omitempty"`
}

type inventoryDependenciesKey struct{}

func (d *inventoryDependencies) merge(other inventoryDependencies) {
	if d.Objects == nil {
		d.Objects = map[string]string{}
	}
	if d.Listings == nil {
		d.Listings = map[string]inventoryListing{}
	}
	for key, version := range other.Objects {
		if old, ok := d.Objects[key]; ok && old != version {
			d.Invalid = true
		}
		d.Objects[key] = version
	}
	for key, listing := range other.Listings {
		d.Listings[key] = listing
	}
	d.Invalid = d.Invalid || other.Invalid
}

func inventoryDependencyKey(key string) bool {
	for _, prefix := range []string{"cache/objects/", "tasks/state/", "solidification/", "generations/", "background/v1/jobs/execution/", "background/v1/receipts/execution/"} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	for _, prefix := range []string{"contents/", "bindings/", "cache/", "proofs/", "lifecycle/", "work/", "content-work/", "projection-revisions/"} {
		if strings.HasPrefix(key, inventory.Prefix+prefix) {
			return true
		}
	}
	return false
}

func recordInventoryDependency(ctx context.Context, key, version string, err error) {
	d, ok := ctx.Value(inventoryDependenciesKey{}).(*inventoryDependencies)
	if !ok || d.Invalid || !inventoryDependencyKey(key) {
		return
	}
	if errors.Is(err, storage.ErrNotFound) {
		version = "missing"
	} else if err != nil || version == "" {
		d.Invalid = true
		return
	}
	if d.Objects == nil {
		d.Objects = map[string]string{}
	}
	if _, exists := d.Objects[key]; !exists && len(d.Objects) >= 4096 {
		d.Invalid = true
		return
	}
	if old, exists := d.Objects[key]; exists && old != version {
		d.Invalid = true
	}
	d.Objects[key] = version
}

func listingFingerprint(entries []storage.Entry, next string) string {
	parts := []string{next}
	for _, entry := range entries {
		parts = append(parts, entry.Key, entry.Version)
	}
	return inventory.ID("listing", parts...)
}

func recordInventoryListing(ctx context.Context, prefix, token string, limit int, entries []storage.Entry, next string, err error, metadata bool) {
	d, ok := ctx.Value(inventoryDependenciesKey{}).(*inventoryDependencies)
	if !ok || d.Invalid || !inventoryDependencyKey(prefix) {
		return
	}
	if err != nil {
		d.Invalid = true
		return
	}
	if d.Listings == nil {
		d.Listings = map[string]inventoryListing{}
	}
	key := inventory.ID("listing", prefix, token, strconv.Itoa(limit), strconv.FormatBool(metadata))
	if _, exists := d.Listings[key]; !exists && len(d.Listings) >= 256 {
		d.Invalid = true
		return
	}
	value := inventoryListing{Prefix: prefix, Token: token, Limit: limit, Fingerprint: listingFingerprint(entries, next), Metadata: metadata}
	if old, exists := d.Listings[key]; exists && old.Fingerprint != value.Fingerprint {
		d.Invalid = true
	}
	d.Listings[key] = value
}
