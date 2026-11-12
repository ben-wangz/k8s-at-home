package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

func TestIntegrityReaderFinalizesBoundedStreams(t *testing.T) {
	valid := []byte("content")
	validDigest := "sha256:ed7002b439e9ac845f22357d822bac1444730fbdb6016d3ec9432297b9ec9f73"
	for _, test := range []struct {
		name       string
		data       []byte
		expected   int64
		wantStream error
		wantFinal  error
	}{
		{name: "valid", data: valid, expected: int64(len(valid))},
		{name: "short", data: valid[:3], expected: int64(len(valid)), wantFinal: ErrDigestMismatch},
		{name: "long", data: append(append([]byte(nil), valid...), 'x'), expected: int64(len(valid)), wantStream: ErrSizeExceeded, wantFinal: ErrSizeExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			digest := validDigest
			if test.name == "short" || test.name == "long" {
				digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
			}
			reader, err := NewIntegrityReader(context.Background(), bytes.NewReader(test.data), digest, test.expected, test.expected)
			if err != nil {
				t.Fatal(err)
			}
			_, streamErr := io.Copy(io.Discard, reader)
			if !errors.Is(streamErr, test.wantStream) {
				t.Fatalf("stream error = %v, want %v", streamErr, test.wantStream)
			}
			finalErr := reader.Finalize()
			if !errors.Is(finalErr, test.wantFinal) {
				t.Fatalf("finalize error = %v, want %v", finalErr, test.wantFinal)
			}
		})
	}
}

func TestIntegrityReaderHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader, err := NewIntegrityReader(ctx, bytes.NewReader([]byte("x")), "sha256:2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("read error = %v, want context canceled", err)
	}
}
