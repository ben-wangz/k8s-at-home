package app

import (
	"context"
	"errors"
	"net/http"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strconv"
	"strings"
	"time"
)

type storageObjectPage struct {
	diagnosticPage[storageObject]
	SnapshotID                         string    `json:"snapshotID"`
	StartedAt                          time.Time `json:"startedAt"`
	CompletedAt                        time.Time `json:"completedAt"`
	ConservativeReclaimableBytesInPage int64     `json:"conservativeReclaimableBytesInPage"`
	PreviewOnly                        bool      `json:"previewOnly"`
	CandidateBytesInPage               int64     `json:"candidateBytesInPage"`
}

func (s *Service) storageSnapshot(ctx context.Context, id string) (storageSnapshot, error) {
	key := storageInventoryPrefix + "current.json"
	if id != "" {
		if !inventory.ValidID(id) {
			return storageSnapshot{}, inventory.ErrInvalid
		}
		key = storageInventoryPrefix + "snapshots/" + id + "/manifest.json"
	}
	snapshot, _, err := inventory.Read[storageSnapshot](ctx, s.store, key)
	if errors.Is(err, storage.ErrNotFound) {
		if id != "" {
			err = inventory.ErrExpired
		} else {
			err = inventory.ErrNotObserved
		}
	}
	if err == nil && !snapshot.ExpiresAt.After(time.Now()) {
		if id != "" {
			err = inventory.ErrExpired
		} else {
			err = inventory.ErrCurrentExpired
		}
	}
	return snapshot, err
}

func (s *Service) serveStorage(writer http.ResponseWriter, request *http.Request, ctx context.Context) {
	values, cursor, limit, err := s.diagnosticQuery(request, storageQueryFields(request.URL.Path)...)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	id := values.Get("snapshotID")
	if strings.HasPrefix(request.URL.Path, "/api/v1/storage/objects/") && id == "" {
		id = strings.Split(strings.TrimPrefix(request.URL.Path, "/api/v1/storage/objects/"), ".")[0]
	}
	if v := values.Get("associated"); v != "" && v != "true" && v != "false" {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	if v := values.Get("sort"); v != "" && v != "objectBytes_desc" {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	if request.URL.Path == "/api/v1/storage/usage" && !validStorageUsageQuery(values) {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	if cursor.SnapshotID != "" {
		if id != "" && id != cursor.SnapshotID {
			writeInventoryError(writer, inventory.ErrInvalid)
			return
		}
		id = cursor.SnapshotID
	}
	snapshot, err := s.storageSnapshot(ctx, id)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	cursor.SnapshotID = snapshot.SnapshotID
	if snapshot.ExpiresAt.Before(cursor.ExpiresAt) {
		cursor.ExpiresAt = snapshot.ExpiresAt
	}
	writer.Header().Set("Cache-Control", "no-store")
	switch request.URL.Path {
	case "/api/v1/storage/summary":
		writeJSON(writer, http.StatusOK, s.storageSummary(ctx, snapshot))
	case "/api/v1/storage/usage":
		s.serveStorageUsage(writer, request, ctx, snapshot, cursor, limit)
	case "/api/v1/storage/objects", "/api/v1/storage/reclaimable":
		s.listStorageObjects(writer, request, ctx, snapshot, cursor, limit)
	default:
		s.getStorageObject(writer, request, ctx, snapshot)
	}
}

func (s *Service) listStorageObjects(writer http.ResponseWriter, request *http.Request, ctx context.Context, snapshot storageSnapshot, cursor diagnosticCursor, limit int) {
	pageIndex, err := strconv.Atoi(cursor.Storage)
	if cursor.Storage == "" {
		pageIndex = 0
		err = nil
	}
	if err != nil || pageIndex < 0 || pageIndex > snapshot.Pages || cursor.Phase < 0 {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	page := storageObjectPage{diagnosticPage: diagnosticPage[storageObject]{Items: []storageObject{}, ObservedAt: snapshot.CompletedAt, Consistency: snapshot.Consistency}, SnapshotID: snapshot.SnapshotID, StartedAt: snapshot.StartedAt, CompletedAt: snapshot.CompletedAt, PreviewOnly: strings.HasSuffix(request.URL.Path, "/reclaimable")}
	if pageIndex < snapshot.Pages {
		objects, _, err := inventory.Read[[]storageObject](ctx, s.store, storagePageKey(snapshot.SnapshotID, pageIndex))
		if err != nil {
			writeInventoryError(writer, err)
			return
		}
		if cursor.Phase > len(objects) {
			writeInventoryError(writer, inventory.ErrInvalid)
			return
		}
		for cursor.Phase < len(objects) && page.Scanned < limit {
			object := objects[cursor.Phase]
			cursor.Phase++
			page.Scanned++
			if matchesStorageObject(object, request) {
				page.Items = append(page.Items, object)
				if object.ReclaimState == "eligible" {
					page.ConservativeReclaimableBytesInPage += object.ObjectBytes
				}
				if object.ReclaimState == "candidate" {
					page.CandidateBytesInPage += object.ObjectBytes
				}
			}
		}
		if cursor.Phase >= len(objects) {
			pageIndex++
			cursor.Phase = 0
		}
	}
	if pageIndex < snapshot.Pages {
		cursor.Storage = strconv.Itoa(pageIndex)
		page.NextToken = s.diagnosticToken(cursor)
	}
	writeJSON(writer, http.StatusOK, page)
}

func matchesStorageObject(object storageObject, request *http.Request) bool {
	values := request.URL.Query()
	fields := map[string]string{"domain": object.Domain, "category": object.Category, "digest": object.Digest, "workID": object.WorkID, "cleanupState": object.CleanupState, "taskID": object.TaskID, "intentID": object.IntentID, "transferID": object.TransferID, "protectionState": object.ProtectionState, "reclaimState": object.ReclaimState}
	for key, value := range fields {
		if values.Get(key) != "" && values.Get(key) != value {
			return false
		}
	}
	if !matchesStorageAssociation(object, values) {
		return false
	}
	if associated := values.Get("associated"); associated != "" && associated != strconv.FormatBool(object.AssociationState == "known") {
		return false
	}
	return true
}

func (s *Service) getStorageObject(writer http.ResponseWriter, request *http.Request, ctx context.Context, snapshot storageSnapshot) {
	id := strings.TrimPrefix(request.URL.Path, "/api/v1/storage/objects/")
	parts := strings.Split(id, ".")
	if len(parts) != 4 || !inventory.ValidID(parts[0]) {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	page, err := strconv.Atoi(parts[1])
	if err != nil || page < 0 {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	offset, err := strconv.Atoi(parts[2])
	if err != nil || offset < 0 || offset >= 32 {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	if snapshot.SnapshotID != parts[0] {
		snapshot, err = s.storageSnapshot(ctx, parts[0])
		if err != nil {
			writeInventoryError(writer, err)
			return
		}
	}
	if page >= snapshot.Pages {
		writeInventoryError(writer, storage.ErrNotFound)
		return
	}
	objects, _, err := inventory.Read[[]storageObject](ctx, s.store, storagePageKey(snapshot.SnapshotID, page))
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	if offset >= len(objects) || objects[offset].ID != id {
		writeInventoryError(writer, storage.ErrNotFound)
		return
	}
	writeJSON(writer, http.StatusOK, objects[offset])
}
