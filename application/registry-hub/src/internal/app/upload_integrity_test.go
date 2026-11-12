package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"registry-hub/internal/artifact"
)

func TestUploadIntegrityReaderVerifiesSinglePass(t *testing.T) {
	data := []byte("payload")
	reader, err := newUploadIntegrityReader(context.Background(), bytes.NewReader(data), artifact.CalculateSHA256(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	read, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(read, data) {
		t.Fatalf("read=%q err=%v", read, err)
	}
	if err := reader.Finalize(); err != nil {
		t.Fatalf("Finalize() = %v", err)
	}
}

func TestUploadIntegrityReaderRejectsLengthAndDigestMismatch(t *testing.T) {
	for _, test := range []struct {
		name   string
		data   []byte
		digest string
		size   int64
	}{
		{name: "short", data: []byte("short"), digest: artifact.CalculateSHA256([]byte("payload")), size: 7},
		{name: "long", data: []byte("payload!"), digest: artifact.CalculateSHA256([]byte("payload!")), size: 7},
		{name: "wrong digest", data: []byte("payload"), digest: artifact.CalculateSHA256([]byte("other")), size: 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader, err := newUploadIntegrityReader(context.Background(), bytes.NewReader(test.data), test.digest, test.size)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, reader)
			if err := reader.Finalize(); !errors.Is(err, artifact.ErrDigestMismatch) {
				t.Fatalf("Finalize() = %v, want digest mismatch", err)
			}
		})
	}
}
