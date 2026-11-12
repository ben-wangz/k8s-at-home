package storage

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"strings"
)

var ErrDigestMismatch = errors.New("storage content digest mismatch")
var ErrSizeExceeded = errors.New("storage content exceeds the configured size limit")

type VerifiedPutOptions struct {
	PutOptions
	Digest string
}

type VerifiedPutter interface {
	PutVerified(context.Context, string, io.Reader, int64, VerifiedPutOptions) (Entry, error)
}

func NewDigestHasher(reference string) (hash.Hash, error) {
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

func DigestFromHasher(reference string, hasher hash.Hash) string {
	algorithm, _, _ := strings.Cut(reference, ":")
	return algorithm + ":" + hex.EncodeToString(hasher.Sum(nil))
}

type IntegrityReader struct {
	ctx          context.Context
	source       io.Reader
	hasher       hash.Hash
	digest       string
	expectedSize int64
	maxBytes     int64
	count        int64
	readErr      error
	finalized    bool
	finalizeErr  error
}

func NewIntegrityReader(ctx context.Context, source io.Reader, digest string, expectedSize, maxBytes int64) (*IntegrityReader, error) {
	if source == nil || expectedSize < 0 || maxBytes < expectedSize {
		return nil, errors.New("integrity reader source or size limit is invalid")
	}
	hasher, err := NewDigestHasher(digest)
	if err != nil {
		return nil, err
	}
	return &IntegrityReader{ctx: ctx, source: source, hasher: hasher, digest: digest, expectedSize: expectedSize, maxBytes: maxBytes}, nil
}

func (r *IntegrityReader) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	if r.finalized {
		return 0, errors.New("integrity reader is already finalized")
	}
	if err := r.ctx.Err(); err != nil {
		r.readErr = err
		return 0, err
	}
	remaining := r.maxBytes - r.count
	if remaining < int64(len(buffer)) {
		buffer = buffer[:remaining+1]
	}
	n, err := r.source.Read(buffer)
	if int64(n) > remaining {
		if remaining > 0 {
			_, _ = r.hasher.Write(buffer[:remaining])
			r.count += remaining
		}
		r.readErr = ErrSizeExceeded
		return int(remaining), ErrSizeExceeded
	}
	if n > 0 {
		_, _ = r.hasher.Write(buffer[:n])
		r.count += int64(n)
	}
	if err != nil {
		r.readErr = err
	}
	return n, err
}

func (r *IntegrityReader) Count() int64 { return r.count }

func (r *IntegrityReader) Finalize() error {
	if r.finalized {
		return r.finalizeErr
	}
	r.finalized = true
	if r.readErr != nil && !errors.Is(r.readErr, io.EOF) {
		r.finalizeErr = r.readErr
		return r.finalizeErr
	}
	if r.count != r.expectedSize {
		r.finalizeErr = ErrDigestMismatch
		return r.finalizeErr
	}
	if !errors.Is(r.readErr, io.EOF) {
		r.finalizeErr = r.confirmEOF()
		if r.finalizeErr != nil {
			return r.finalizeErr
		}
	}
	if DigestFromHasher(r.digest, r.hasher) != r.digest {
		r.finalizeErr = ErrDigestMismatch
	}
	return r.finalizeErr
}

func (r *IntegrityReader) confirmEOF() error {
	var extra [1]byte
	for attempt := 0; attempt < 100; attempt++ {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		n, err := r.source.Read(extra[:])
		if n > 0 {
			return ErrSizeExceeded
		}
		if errors.Is(err, io.EOF) {
			r.readErr = io.EOF
			return nil
		}
		if err != nil {
			r.readErr = err
			return err
		}
	}
	return io.ErrNoProgress
}
