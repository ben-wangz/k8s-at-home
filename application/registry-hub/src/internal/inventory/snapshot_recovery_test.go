package inventory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"registry-hub/internal/storage"
	"strings"
	"testing"
	"time"
)

type snapshotFaultStore struct {
	storage.Store
	failKey    string
	unknown    bool
	failRead   bool
	emptyFirst bool
}

func (s *snapshotFaultStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if key != s.failKey {
		return s.Store.Put(ctx, key, body, size, options)
	}
	s.failKey = ""
	if !s.unknown {
		return storage.Entry{}, storage.ErrTimeout
	}
	entry, err := s.Store.Put(ctx, key, body, size, options)
	if err != nil {
		return entry, err
	}
	if s.failRead {
		s.failKey = "read:" + key
	}
	return storage.Entry{}, storage.ErrUnknownCommit
}

func (s *snapshotFaultStore) Get(ctx context.Context, key string) (storage.Object, error) {
	if s.failKey == "read:"+key {
		s.failKey = ""
		return storage.Object{}, storage.ErrTimeout
	}
	return s.Store.Get(ctx, key)
}

func (s *snapshotFaultStore) List(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	if s.emptyFirst && strings.HasSuffix(prefix, "/captures/") && token == "" {
		s.emptyFirst = false
		return nil, "empty-page", nil
	}
	if token == "empty-page" {
		token = ""
	}
	return s.Store.List(ctx, prefix, token, limit)
}

func TestSnapshotAssemblyResumesWithoutRecountingOrReadingLiveSources(t *testing.T) {
	for _, failure := range []string{"page", "checkpoint", "page_unknown", "checkpoint_unknown", "empty_scan"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			repo := testRepository(t)
			fault := &snapshotFaultStore{Store: repo.Store}
			repo.Store = fault
			writer := repo.NewWriter(time.Now().UTC(), time.Hour, 1)
			for n := 0; n < 5; n++ {
				image := Image{ID: ID("reference", fmt.Sprint(n)), Scope: "reference", Category: "unknown"}
				if err := writer.AddImage(ctx, image, []Object{{ID: fmt.Sprint(n)}}, nil); err != nil {
					t.Fatal(err)
				}
			}
			fault.emptyFirst = failure == "empty_scan"
			// Seal and advance once before injecting failures into a later batch.
			if done, err := writer.Assemble(ctx, 2); err != nil || done {
				t.Fatalf("initial assembly: done=%v err=%v", done, err)
			}
			if failure == "empty_scan" {
				if fault.emptyFirst {
					t.Fatal("empty continuation page was not scanned")
				}
			} else if strings.HasPrefix(failure, "page") {
				fault.failKey = writer.key("reference/00000001.json")
			} else {
				fault.failKey = writer.key("assembly.json")
			}
			fault.unknown, fault.failRead = strings.HasSuffix(failure, "unknown"), true
			stale := writer.Snapshot
			_, firstErr := writer.Assemble(ctx, 2)
			if failure != "empty_scan" && firstErr == nil {
				t.Fatal("injected assembly failure was not observed")
			}
			// The app checkpoint may lag behind committed assembly pages and counts.
			writer = repo.ResumeWriter(stale)
			for n := 0; n < 20; n++ {
				done, err := writer.Assemble(ctx, 2)
				if err != nil {
					t.Fatal(err)
				}
				if done {
					break
				}
				if n == 19 {
					t.Fatal("assembly did not converge")
				}
			}
			if err := writer.Publish(ctx, true, nil); err != nil {
				t.Fatal(err)
			}
			q := Query{Kind: "images", Scope: "reference", Limit: 100, Filters: url.Values{}}
			page, err := repo.Images(ctx, q, nil, time.Minute)
			if err != nil || len(page.Items) != 5 || page.Counts["reference:unknown"] != 5 {
				t.Fatalf("recovered page=%+v err=%v", page, err)
			}
			seen := map[string]bool{}
			for _, image := range page.Items {
				if seen[image.ID] {
					t.Fatal("member counted more than once")
				}
				seen[image.ID] = true
			}
		})
	}
}

func TestSnapshotFailedCaptureCannotPublishLooseEntry(t *testing.T) {
	ctx := context.Background()
	repo := testRepository(t)
	writer := repo.NewWriter(time.Now().UTC(), time.Hour, 1)
	image := Image{ID: ID("reference", "retry"), Scope: "reference", Category: "unknown"}
	fault := &snapshotFaultStore{Store: repo.Store, failKey: writer.key("members/" + image.ID + ".json")}
	repo.Store = fault
	if err := writer.AddImage(ctx, image, nil, nil); !errors.Is(err, storage.ErrTimeout) {
		t.Fatalf("capture failure: %v", err)
	}
	if ready, err := writer.HasImage(ctx, image.ID); err != nil || ready {
		t.Fatalf("incomplete member marked ready: %v %v", ready, err)
	}
	if err := writer.AddImage(ctx, image, nil, nil); err != nil {
		t.Fatalf("capture replay: %v", err)
	}
	assembleTestSnapshot(t, writer)
	if err := writer.Publish(ctx, true, nil); err != nil {
		t.Fatal(err)
	}
	if writer.Snapshot.Counts["reference:unknown"] != 1 {
		t.Fatal("capture replay counted twice")
	}
}

func TestSnapshotAssemblyRejectsInterruptedCapture(t *testing.T) {
	ctx := context.Background()
	repo := testRepository(t)
	writer := repo.NewWriter(time.Now().UTC(), time.Hour, 1)
	image := Image{ID: ID("reference", "interrupted"), Scope: "reference", Category: "unknown"}
	repo.Store = &snapshotFaultStore{Store: repo.Store, failKey: writer.key("objects/" + image.ID + "/count.json")}
	if err := writer.AddImage(ctx, image, nil, nil); !errors.Is(err, storage.ErrTimeout) {
		t.Fatalf("capture failure: %v", err)
	}
	if _, err := writer.Assemble(ctx, 1); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("unfinished capture assembled: %v", err)
	}
	if err := writer.Publish(ctx, false, nil); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("unfinished capture published: %v", err)
	}
}
