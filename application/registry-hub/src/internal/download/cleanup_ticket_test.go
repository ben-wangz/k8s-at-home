package download

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
)

func completedCleanupFixture(t *testing.T) (context.Context, *Manager, Request, CleanupTicket) {
	t.Helper()
	ctx, store := testDownloadStore(t)
	m := testManager(store)
	content := []byte("abcdefgh")
	request := testRequest(content, artifact.CalculateSHA256(content), 1, func(context.Context, int64, int64, string) (*http.Response, context.CancelFunc, error) {
		return testResponse(http.StatusOK, content, int64(len(content)), io.NopCloser(bytes.NewReader(content))), func() {}, nil
	})
	if _, err := m.Fetch(ctx, request); err != nil {
		t.Fatal(err)
	}
	ticket, err := m.CompletedTicket(ctx, request.Identity, request.Digest, request.OwnerFence)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, m, request, ticket
}

func finishTicket(t *testing.T, ctx context.Context, m *Manager, ticket CleanupTicket) {
	t.Helper()
	p := CleanupProgress{}
	for n := 0; n < 32; n++ {
		done, err := m.CleanupCommittedStep(ctx, ticket, &p, func(context.Context) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if done {
			return
		}
	}
	t.Fatal("cleanup did not converge")
}

func TestOldCleanupTicketCannotRemoveResumedOrNewSession(t *testing.T) {
	for _, mode := range []string{"resumed", "new"} {
		t.Run(mode, func(t *testing.T) {
			ctx, m, request, ticket := completedCleanupFixture(t)
			if mode == "new" {
				finishTicket(t, ctx, m, ticket)
			}
			request.OwnerFence++
			if _, err := m.Fetch(ctx, request); err != nil {
				t.Fatal(err)
			}
			current, err := m.CompletedTicket(ctx, request.Identity, request.Digest, request.OwnerFence)
			if err != nil {
				t.Fatal(err)
			}
			finishTicket(t, ctx, m, ticket)
			body, _, err := m.Open(ctx, request.Identity, request.Digest, request.MaxBytes)
			if err != nil {
				t.Fatalf("current session lost: %v", err)
			}
			data, err := io.ReadAll(body)
			_ = body.Close()
			if err != nil || string(data) != "abcdefgh" {
				t.Fatalf("current content=%q err=%v", data, err)
			}
			if reserved, count, err := m.Usage(ctx); err != nil || reserved == 0 || count != 1 {
				t.Fatalf("current quota=%d count=%d err=%v", reserved, count, err)
			}
			finishTicket(t, ctx, m, current)
			if reserved, count, err := m.Usage(ctx); err != nil || reserved != 0 || count != 0 {
				t.Fatalf("final quota=%d count=%d err=%v", reserved, count, err)
			}
		})
	}
}

func TestCommittedCleanupRequiresValidGuardBeforeDeleting(t *testing.T) {
	ctx, m, request, ticket := completedCleanupFixture(t)
	p := CleanupProgress{}
	_, err := m.CleanupCommittedStep(ctx, ticket, &p, func(context.Context) error { return storage.ErrLeaseLost })
	if !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("stale guard=%v", err)
	}
	body, _, err := m.Open(ctx, request.Identity, request.Digest, request.MaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	_ = body.Close()
}
