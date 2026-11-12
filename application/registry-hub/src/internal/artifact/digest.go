package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"registry-hub/internal/storage"
)

var ErrDigestMismatch = errors.New("content digest mismatch")

func Hasher(reference string) (hash.Hash, error) {
	return storage.NewDigestHasher(reference)
}

func VerifyDigest(reference string, data []byte) error {
	hasher, err := Hasher(reference)
	if err != nil {
		return err
	}
	_, _ = hasher.Write(data)
	actual := storage.DigestFromHasher(reference, hasher)
	if actual != reference {
		return ErrDigestMismatch
	}
	return nil
}

func CalculateSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func DigestFromHasher(reference string, hasher hash.Hash) string {
	return storage.DigestFromHasher(reference, hasher)
}
