package filesystem

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"registry-hub/internal/storage"
	"time"
)

func hashReader(ctx context.Context, reader io.Reader) (string, error) {
	hash := sha256.New()
	started := time.Now()
	count, err := io.Copy(hash, storage.ContextReader{Context: ctx, Reader: reader})
	storage.ObserveIO(ctx, "read", count, time.Since(started))
	storage.ObserveIO(ctx, "hash", count, time.Since(started))
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
