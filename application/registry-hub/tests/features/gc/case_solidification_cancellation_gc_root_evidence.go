package gc

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func rootCompletionFailureDiagnostics(ctx context.Context, environment testkit.CaseContext, repository, reference string, since time.Time, previousID, previousRound string, manifest support.Manifest, podmanPull, newBlobTask map[string]any, failure error) map[string]any {
	descriptors := make([]support.Descriptor, 0, len(manifest.Layers)+1)
	if manifest.Config != nil {
		descriptors = append(descriptors, *manifest.Config)
	}
	descriptors = append(descriptors, manifest.Layers...)
	digests := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		digests = append(digests, descriptor.Digest)
	}

	evidence := map[string]any{
		"failure":                 failure.Error(),
		"evidenceType":            "read_only_no_blob_fetch",
		"evidenceWindowStart":     since.UTC().Format(time.RFC3339Nano),
		"podmanPull":              podmanPull,
		"newBlobTask":             summarizeBlobTask(newBlobTask),
		"rootTaskHistory":         []map[string]any{},
		"blobTasks":               map[string]any{},
		"blobIntents":             map[string]any{},
		"clientRouting":           clientRoutingEvidence(ctx, environment),
		"backendRequestLogs":      []string{},
		"distributionRequestLogs": []string{},
		"fixtureBlobRequests":     map[string]fixtureBlobRequestEvidence{},
	}

	rootHistory := make([]map[string]any, 0, 2)
	previous, err := taskDetailByID(ctx, environment, previousID)
	if err != nil {
		rootHistory = append(rootHistory, map[string]any{"role": "previous", "taskID": previousID, "error": err.Error()})
	} else {
		rootHistory = append(rootHistory, map[string]any{"role": "previous", "task": summarizeRootTask(previous)})
	}
	current, err := taskDetailForNewRoot(ctx, environment, repository, reference, since, previousID, previousRound)
	if err != nil {
		rootHistory = append(rootHistory, map[string]any{"role": "replacement", "error": err.Error()})
	} else {
		rootHistory = append(rootHistory, map[string]any{"role": "replacement", "task": summarizeRootTask(current)})
	}
	evidence["rootTaskHistory"] = rootHistory

	blobTasks := make(map[string]any, len(descriptors))
	blobIntents := make(map[string]any, len(descriptors))
	for _, descriptor := range descriptors {
		digest := descriptor.Digest
		if testkit.String(newBlobTask["blobDigest"]) == digest {
			blobTasks[digest] = map[string]any{"size": descriptor.Size, "task": summarizeBlobTask(newBlobTask)}
		} else {
			task, taskErr := taskDetailForBlobDigest(ctx, environment, repository, digest, since, "")
			if taskErr != nil {
				blobTasks[digest] = map[string]any{"size": descriptor.Size, "error": taskErr.Error()}
			} else {
				blobTasks[digest] = map[string]any{"size": descriptor.Size, "task": summarizeBlobTask(task)}
			}
		}
		intent, intentErr := blobIntentEvidenceForDigest(ctx, environment, repository, digest)
		if intentErr != nil {
			blobIntents[digest] = map[string]any{"error": intentErr.Error()}
		} else {
			blobIntents[digest] = intent
		}
	}
	evidence["blobTasks"] = blobTasks
	evidence["blobIntents"] = blobIntents

	requestLogs, err := backendRegistryRequestLogs(ctx, environment, since)
	if err != nil {
		evidence["backendRequestLogError"] = err.Error()
	} else {
		evidence["backendRequestLogs"] = requestLogs
	}
	distributionRequests, err := distributionRequestEvidence(ctx, environment, repository, reference, digests, since)
	if err != nil {
		evidence["distributionRequestLogError"] = err.Error()
	} else {
		evidence["distributionRequestLogs"] = distributionRequests
	}
	fixtureRequests, err := fixtureBlobRequestEvidenceFor(ctx, environment, digests...)
	if err != nil {
		evidence["fixtureRequestLogError"] = err.Error()
	} else {
		evidence["fixtureBlobRequests"] = fixtureRequests
	}
	status, err := gcStatus(ctx, environment)
	if err != nil {
		evidence["gcStatusError"] = err.Error()
	} else {
		evidence["gcStatus"] = status
	}
	return evidence
}

func summarizeRootTask(task map[string]any) map[string]any {
	fields := []string{
		"id", "kind", "status", "phase", "authority", "repository", "reference", "rootDigest",
		"generation", "roundID", "attemptID", "retryCount", "nextAttemptAt", "lastProgressAt", "updatedAt",
		"waitReason", "waitResourceKey", "rootPublishing", "rootPublished", "gcProtected",
		"protectionSessionID", "sessionIDs", "protectionSessions", "releasedProtectionSessionIDs",
		"cancelRequested", "cancelPhase", "terminalCleanupComplete", "recoveryReason", "errorClass", "errorMessage", "lastError",
	}
	summary := make(map[string]any, len(fields))
	for _, field := range fields {
		if value, ok := task[field]; ok {
			summary[field] = value
		}
	}
	return summary
}

func distributionRequestEvidence(ctx context.Context, environment testkit.CaseContext, repository, reference string, digests []string, since time.Time) ([]string, error) {
	lines, err := distributionLogs(ctx, environment, since)
	if err != nil {
		return nil, err
	}
	repoPath := repositoryPath(repository)
	requests := make([]string, 0, 30)
	for _, line := range lines {
		if !strings.Contains(line, `http.request.uri=`) {
			continue
		}
		matches := repoPath != "" && strings.Contains(line, repoPath) || strings.Contains(line, url.PathEscape(reference))
		for _, digest := range digests {
			matches = matches || strings.Contains(line, digest)
		}
		if !matches {
			continue
		}
		if len(line) > 4096 {
			line = line[:4096] + "[truncated]"
		}
		requests = append(requests, line)
		if len(requests) == 30 {
			break
		}
	}
	return requests, nil
}
