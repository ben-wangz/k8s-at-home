package workqueue

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

type summaryProgress struct{ Count int }

func (p summaryProgress) CompletionSummary(Job) (json.RawMessage, error) {
	return json.Marshal(map[string]any{"schemaVersion": 1, "processedSteps": p.Count})
}

type receiptConflict struct {
	storage.Store
	conflicted bool
}

func (s *receiptConflict) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if strings.Contains(key, "/receipts/") && !s.conflicted {
		s.conflicted = true
		return storage.Entry{}, storage.ErrConditionFailed
	}
	return s.Store.Put(ctx, key, body, size, options)
}

func TestFinalSummarySurvivesUnknownAndConflictingArchiveWrites(t *testing.T) {
	for _, mode := range []string{"unknown", "conflict"} {
		t.Run(mode, func(t *testing.T) {
			q := queueFixture(t)
			if mode == "unknown" {
				q.Store = &unknownCommitStore{Store: q.Store}
			} else {
				q.Store = &receiptConflict{Store: q.Store}
			}
			ctx := context.Background()
			job, err := q.Enqueue(ctx, "terminal_cleanup", "summary", "source", time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			job, err = q.Claim(ctx, job)
			if err != nil {
				t.Fatal(err)
			}
			err = q.Finish(ctx, &job, summaryProgress{Count: 83}, true, nil)
			if mode == "unknown" && err != nil || mode == "conflict" && !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("archive result: %v", err)
			}
			// Recreate the queue after a failed archive acknowledgment.
			q = Queue{Store: q.Store}
			current, err := q.Read(ctx, job.Kind, job.ID)
			if err != nil || current.State != "done" || string(current.FinalSummary) != `{"processedSteps":83,"schemaVersion":1}` {
				t.Fatalf("lost summary: %+v %v", current, err)
			}
			if err := q.Archive(ctx, current); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFinalSummaryCannotUpgradeReceiptWithoutActiveEvidence(t *testing.T) {
	q := queueFixture(t)
	ctx := context.Background()
	job, _ := q.Enqueue(ctx, "terminal_cleanup", "legacy", "source", time.Now().UTC())
	job, _ = q.Claim(ctx, job)
	if err := q.Finish(ctx, &job, nil, true, nil); err != nil {
		t.Fatal(err)
	}
	job.Progress = json.RawMessage(`{"processed":999}`)
	job.FinalSummary = json.RawMessage(`{"processedSteps":999}`)
	if err := q.Archive(ctx, job); err != nil {
		t.Fatal(err)
	}
	receipt, _, err := inventory.Read[Job](ctx, q.Store, ReceiptKey(job.Kind, job.ID))
	if err != nil || len(receipt.FinalSummary) != 0 {
		t.Fatalf("invented legacy counters: %+v %v", receipt, err)
	}
}

func TestFinalSummaryRejectsOversizedOrNonObjectData(t *testing.T) {
	q := queueFixture(t)
	for _, summary := range []string{`[]`, `null`, `{"value":"` + strings.Repeat("x", MaxFinalSummaryBytes) + `"}`} {
		job := Job{FinalSummary: json.RawMessage(summary)}
		if err := q.summarize(&job); !errors.Is(err, storage.ErrCorruptObject) {
			t.Fatalf("accepted invalid summary: %v", err)
		}
	}
}
