package app

import (
	"bytes"
	"context"
	"net/http"
	"registry-hub/internal/artifact"
	"sync"
	"testing"
	"time"
)

func TestProtectionRecoveryResumesRemoteBlobHashAcrossOwners(t *testing.T) {
	f, _ := newRecoveryFixture(t)
	payload := bytes.Repeat([]byte("verified blob content"), 500000)
	digest := artifact.CalculateSHA256(payload)
	var mu sync.Mutex
	ranges := []string{}
	f.service.registry = newLocalRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		if req.Method == http.MethodGet {
			ranges = append(ranges, req.Header.Get("Range"))
		}
		mu.Unlock()
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, req, "blob", time.Time{}, bytes.NewReader(payload))
	}))
	ctx := context.Background()
	var lastOffset int64
	for step := 0; step < 10; step++ {
		r, work, release, err := f.service.openProtectionRecovery(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if step == 0 {
			r.checkpoint.Phase = "remote_operations"
			r.checkpoint.Remote.Write = &protectionRemoteWrite{Kind: "blob", Path: "/v2/recovery/blobs/" + digest, Digest: digest, Size: int64(len(payload))}
		}
		if r.checkpoint.Remote.Offset != lastOffset {
			t.Fatal("blob verification offset was lost on takeover")
		}
		if err := r.verifyRemoteBlobChunk(work); err != nil {
			_ = release()
			t.Fatal(err)
		}
		if err := r.save(work); err != nil {
			_ = release()
			t.Fatal(err)
		}
		if err := release(); err != nil {
			t.Fatal(err)
		}
		lastOffset = r.checkpoint.Remote.Offset
		if r.checkpoint.Remote.Verified {
			mu.Lock()
			defer mu.Unlock()
			if len(ranges) != 3 || ranges[0] != "bytes=0-4194303" || ranges[1] != "bytes=4194304-8388607" || lastOffset != int64(len(payload)) {
				t.Fatalf("blob was replayed or verification incomplete: %v offset=%d", ranges, lastOffset)
			}
			return
		}
	}
	t.Fatal("remote verification did not converge")
}
