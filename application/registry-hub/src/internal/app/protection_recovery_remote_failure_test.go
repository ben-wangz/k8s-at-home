package app

import (
	"context"
	"net/http"
	"strconv"
	"testing"
)

func TestProtectionRecoveryRetainsUnknownAndCorruptRemoteWrites(t *testing.T) {
	for _, mode := range []string{"forbidden", "unavailable", "untrusted_manifest_miss", "corrupt_blob", "missing_identity"} {
		t.Run(mode, func(t *testing.T) {
			f, _ := newRecoveryFixture(t)
			write := recoveryAbsentWrite("blob")
			if mode == "untrusted_manifest_miss" {
				write = recoveryAbsentWrite("manifest")
			}
			id := admitInterruptedRecoveryWrite(t, f, write)
			ctx := context.Background()
			if mode == "missing_identity" {
				if err := f.service.gate.updateProtectionSession(ctx, id, func(session *protectionSessionRecord) error {
					session.RemoteWrite = nil
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			f.service.registry = newLocalRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "forbidden":
					w.WriteHeader(http.StatusForbidden)
				case "unavailable":
					w.WriteHeader(http.StatusServiceUnavailable)
				case "untrusted_manifest_miss":
					http.Error(w, "proxy error", http.StatusNotFound)
				case "corrupt_blob":
					w.Header().Set("Docker-Content-Digest", write.Digest)
					w.Header().Set("Content-Length", strconv.FormatInt(write.Size, 10))
					if r.Method == http.MethodGet {
						_, _ = w.Write([]byte("incorrect payload"))
					}
				default:
					t.Error("unknown write reached Distribution")
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			if err := f.service.recoverSolidificationProtection(ctx); err == nil {
				t.Fatal("unsafe remote result completed recovery")
			}
			session, active, err := f.service.gate.protectionSession(ctx, id)
			if err != nil || !active || !session.UncertainRemoteResult || !f.service.gate.State().RecoveryBarrier {
				t.Fatalf("failed verification lost protection: %+v active=%t err=%v", session, active, err)
			}
		})
	}
}
