package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"registry-hub/internal/storage"
)

type referrerCursor struct {
	Repository   string    `json:"repository"`
	Subject      string    `json:"subject"`
	ArtifactType string    `json:"artifactType"`
	IndexDigest  string    `json:"indexDigest"`
	LastDigest   string    `json:"lastDigest"`
	ExpiresAt    time.Time `json:"expiresAt"`
}

func loadCursorKey(ctx context.Context, store storage.Store) ([]byte, error) {
	object, err := store.Get(ctx, "system/referrers/cursor-key")
	if err == nil {
		defer object.Body.Close()
		key, readErr := io.ReadAll(io.LimitReader(object.Body, 64))
		if readErr != nil || len(key) != sha256.Size {
			return nil, errors.New("referrer cursor signing key is invalid")
		}
		return key, nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}
	key := make([]byte, sha256.Size)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if _, err := store.Put(ctx, "system/referrers/cursor-key", bytes.NewReader(key), int64(len(key)), storage.PutOptions{IfAbsent: true}); err != nil {
		if !errors.Is(err, storage.ErrAlreadyExists) && !errors.Is(err, storage.ErrConditionFailed) {
			return nil, err
		}
		object, getErr := store.Get(ctx, "system/referrers/cursor-key")
		if getErr != nil {
			return nil, getErr
		}
		defer object.Body.Close()
		key, readErr := io.ReadAll(io.LimitReader(object.Body, 64))
		if readErr != nil || len(key) != sha256.Size {
			return nil, errors.New("referrer cursor signing key is invalid")
		}
		return key, nil
	}
	return key, nil
}

func signReferrerCursor(key []byte, cursor referrerCursor) (string, error) {
	payload, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func verifyReferrerCursor(key []byte, value, repository, subject, artifactType string) (referrerCursor, error) {
	encoded, signature, found := strings.Cut(value, ".")
	if !found || len(value) > 4096 {
		return referrerCursor{}, errors.New("referrer cursor is malformed")
	}
	actualSignature, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return referrerCursor{}, errors.New("referrer cursor signature is malformed")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(encoded))
	if !hmac.Equal(actualSignature, mac.Sum(nil)) {
		return referrerCursor{}, errors.New("referrer cursor signature is invalid")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return referrerCursor{}, errors.New("referrer cursor payload is malformed")
	}
	var cursor referrerCursor
	if err := json.Unmarshal(payload, &cursor); err != nil || cursor.Repository != repository || cursor.Subject != subject || cursor.ArtifactType != artifactType || cursor.LastDigest == "" || cursor.IndexDigest == "" || !cursor.ExpiresAt.After(time.Now()) {
		return referrerCursor{}, errors.New("referrer cursor does not match this query")
	}
	return cursor, nil
}
