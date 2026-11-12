package tasks

import (
	"testing"
	"time"
)

func TestQuotaTransitionsIgnoreUnrelatedTerminalTasks(t *testing.T) {
	for _, status := range []State{Solidified, Failed, Cancelled} {
		t.Run(string(status), func(t *testing.T) {
			task := Task{Status: status, CancelRequested: status == Cancelled}
			if markQuotaTransition(&task) || task.QuotaWakeRevision != 0 {
				t.Fatalf("unrelated terminal task created quota wake: %+v", task)
			}
		})
	}
}

func TestQuotaTransitionsRetainAdmissionAndReleaseSignals(t *testing.T) {
	for name, task := range map[string]Task{
		"quota_wait":   {Status: WaitingQuota, WaitReason: "distribution_quota", QuotaWaitAt: time.Now()},
		"legacy_wait":  {Status: WaitingQuota, WaitReason: "distribution_quota"},
		"session":      {Status: Running, ProtectionSessionID: "session"},
		"session_set":  {Status: Running, SessionIDs: []string{"session"}},
		"distribution": {Status: Running, ReservedBytes: 1},
		"cache":        {Status: Running, ProtectedCacheBytes: 1},
	} {
		t.Run(name, func(t *testing.T) {
			if !markQuotaTransition(&task) || task.QuotaWakeRevision != 1 {
				t.Fatalf("missing capacity transition: %+v", task)
			}
			if markQuotaTransition(&task) {
				t.Fatal("unchanged capacity state generated another wake")
			}
			task.Status = Solidified
			task.QuotaWaitAt, task.QuotaWaitReason = time.Time{}, ""
			task.ProtectionSessionID, task.SessionIDs = "", nil
			task.ReservedBytes, task.ProtectedCacheBytes = 0, 0
			task.TerminalCleanupComplete = true
			if !markQuotaTransition(&task) || task.QuotaWakeRevision != 2 {
				t.Fatalf("released capacity lost successor signal: %+v", task)
			}
		})
	}
}
