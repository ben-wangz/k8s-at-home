package inventory

import (
	"context"
	"fmt"
	"net/url"
	"registry-hub/internal/storage"
	"strings"
	"testing"
	"time"
)

func TestSnapshotStepAssemblyRecoversPagesAndUnknownCheckpoints(t *testing.T) {
	for _, failure := range []string{"page", "checkpoint", "page_unknown", "checkpoint_unknown", "empty_scan"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			repo := testRepository(t)
			fault := &snapshotFaultStore{Store: repo.Store}
			repo.Store = fault
			writer := repo.NewWriter(time.Now().UTC(), time.Hour, 1)
			for n := 0; n < 5; n++ {
				image := Image{ID: ID("reference", fmt.Sprint(n)), Scope: "reference", Category: "unknown"}
				if err := writer.AddImage(ctx, image, nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			fault.emptyFirst = failure == "empty_scan"
			fault.failKey = writer.key("assembly.json")
			if strings.HasPrefix(failure, "page") {
				fault.failKey = writer.key("reference/00000000.json")
			}
			if failure == "empty_scan" {
				fault.failKey = ""
			}
			fault.unknown, fault.failRead = strings.HasSuffix(failure, "unknown"), true
			lagging, failed, finished := writer.Snapshot, false, false
			for n := 0; n < 100; n++ {
				done, err := writer.AssembleStep(ctx, 2)
				if err != nil {
					failed = true
				}
				if err == nil && done {
					finished = true
					break
				}
				writer = repo.ResumeWriter(lagging)
			}
			if !finished || failure != "empty_scan" && !failed {
				t.Fatalf("assembly recovery: finished=%v failed=%v", finished, failed)
			}
			if err := writer.Publish(ctx, true, nil); err != nil {
				t.Fatal(err)
			}
			page, err := repo.Images(ctx, Query{Kind: "images", Scope: "reference", Limit: 100, Filters: url.Values{}}, nil, time.Minute)
			if err != nil || len(page.Items) != 5 || page.Counts["reference:unknown"] != 5 {
				t.Fatalf("replayed list=%+v err=%v", page, err)
			}
			if _, _, err := Read[assemblyState](ctx, repo.Store, writer.key("assembly.json")); err != nil && err != storage.ErrNotFound {
				t.Fatal(err)
			}
		})
	}
}
