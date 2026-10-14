package artifact

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"hash"
	"strings"
)

var ErrDigestMismatch = errors.New("content digest mismatch")

func Hasher(reference string) (hash.Hash, error) {
	algorithm, encoded, found := strings.Cut(reference, ":")
	if !found || len(encoded) < 32 || len(encoded)%2 != 0 {
		return nil, errors.New("digest is malformed")
	}
	if _, err := hex.DecodeString(encoded); err != nil {
		return nil, errors.New("digest encoding is invalid")
	}
	switch algorithm {
	case "sha256":
		return sha256.New(), nil
	case "sha384":
		return sha512.New384(), nil
	case "sha512":
		return sha512.New(), nil
	default:
		return nil, errors.New("digest algorithm is unsupported")
	}
}

func VerifyDigest(reference string, data []byte) error {
	hasher, err := Hasher(reference)
	if err != nil {
		return err
	}
	_, _ = hasher.Write(data)
	actual := strings.SplitN(reference, ":", 2)[0] + ":" + hex.EncodeToString(hasher.Sum(nil))
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
	algorithm := strings.SplitN(reference, ":", 2)[0]
	return algorithm + ":" + hex.EncodeToString(hasher.Sum(nil))
}
