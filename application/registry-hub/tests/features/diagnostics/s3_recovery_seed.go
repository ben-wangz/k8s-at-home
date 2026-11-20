package diagnostics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func diagnosticsS3Client(ctx context.Context, environment testkit.CaseContext) (*s3.Client, string, error) {
	secret, data, err := support.GetSecret(ctx, environment.Namespace, environment.Release+diagnosticsS3SecretSuffix)
	if err != nil {
		return nil, "", err
	}
	if secret.Labels["registry-hub.io/test-id"] != environment.RunID ||
		secret.Labels["registry-hub.io/instance"] != environment.Fullname ||
		secret.Labels["registry-hub.io/managed-by"] != "registry-hub-test-fixture" {
		return nil, "", fmt.Errorf("diagnostics S3 credential Secret ownership is invalid")
	}
	accessKey, secretKey := string(data["AWS_ACCESS_KEY_ID"]), string(data["AWS_SECRET_ACCESS_KEY"])
	if accessKey == "" || secretKey == "" {
		return nil, "", fmt.Errorf("diagnostics S3 credential Secret is incomplete")
	}
	serviceName := environment.Release + diagnosticsS3SecretSuffix
	service, err := testkit.KubectlGet(ctx, environment.Namespace, "service", serviceName)
	if err != nil {
		return nil, "", err
	}
	clusterIP := testkit.String(testkit.Map(service["spec"])["clusterIP"])
	if clusterIP == "" || clusterIP == "None" {
		return nil, "", fmt.Errorf("diagnostics S3 fixture has no ClusterIP")
	}
	config, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
		awsconfig.WithHTTPClient(&http.Client{Timeout: 20 * time.Second}),
	)
	if err != nil {
		return nil, "", err
	}
	client := s3.NewFromConfig(config, func(options *s3.Options) {
		options.BaseEndpoint = aws.String("http://" + clusterIP + ":8080")
		options.UsePathStyle = true
	})
	return client, "rh-" + environment.RunID, nil
}

func seedResumablePendingBatch(ctx context.Context, client *s3.Client, bucket, snapshotID, runID string) (seededPendingBatch, []byte, error) {
	rows := []seededUsageRow{{Domain: "state", Dimension: "category", Name: "seeded", ObjectBytes: 13, Objects: 1, LogicalUnknown: true, Attribution: "seeded pending batch"}}
	payload, err := json.Marshal(rows)
	if err != nil {
		return seededPendingBatch{}, nil, err
	}
	manifest := seededUsageManifest{Pages: 1, Rows: len(rows)}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return seededPendingBatch{}, nil, err
	}
	sum := sha256.Sum256(payload)
	payloadDigest := hex.EncodeToString(sum[:])
	manifestSum := sha256.Sum256(manifestData)
	manifestDigest := hex.EncodeToString(manifestSum[:])
	run := seededUsageRun{Stage: "reduce"}
	batchID := storageSeedID("batch", runID)
	storageBase := diagnosticsStorageKeyPrefix + "snapshots/" + snapshotID + "/usage/"
	objectBase := diagnosticsS3ObjectPrefix + "snapshots/" + snapshotID + "/usage/"
	runPrefix := seededUsageRunPrefix(diagnosticsStorageKeyPrefix, snapshotID, run)
	pending := seededPendingBatch{
		Version: 1, BatchID: batchID, SnapshotID: snapshotID, Run: run,
		NextCursor: seededCursor{Done: true}, NextPhase: 3,
		PayloadKey: storageBase + "pending/" + batchID + ".json", PayloadSHA256: payloadDigest,
		PageKey: runPrefix + "00000000.json", PageSHA256: payloadDigest,
		ManifestKey: runPrefix + "manifest.json", Manifest: manifest, ManifestSHA256: manifestDigest,
		PreparedAt: time.Now().UTC(),
	}
	state := newSeededStorageCollection(snapshotID)
	state.AttributionPhase = 2
	state.UsagePending = &pending
	if err := putS3Object(ctx, client, bucket, objectBase+"pending/"+batchID+".json", payload); err != nil {
		return seededPendingBatch{}, nil, err
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return seededPendingBatch{}, nil, err
	}
	if err := putS3Object(ctx, client, bucket, diagnosticsS3ObjectPrefix+"checkpoint.json", encoded); err != nil {
		return seededPendingBatch{}, nil, err
	}
	if err := seedStorageInventoryDue(ctx, client, bucket); err != nil {
		return seededPendingBatch{}, nil, err
	}
	return pending, payload, nil
}

func seedLegacyStorageOutput(ctx context.Context, client *s3.Client, bucket, snapshotID string, withManifest bool) error {
	state := newSeededStorageCollection(snapshotID)
	state.AttributionPhase = 2
	checkpoint, err := json.Marshal(state)
	if err != nil {
		return err
	}
	runPrefix := seededUsageRunPrefix(diagnosticsS3ObjectPrefix, snapshotID, seededUsageRun{Stage: "reduce"})
	if err := putS3Object(ctx, client, bucket, runPrefix+"00000000.json", []byte("[]")); err != nil {
		return err
	}
	if withManifest {
		if err := putS3Object(ctx, client, bucket, runPrefix+"manifest.json", []byte(`{"pages":1,"rows":0}`)); err != nil {
			return err
		}
	}
	if err := putS3Object(ctx, client, bucket, diagnosticsS3ObjectPrefix+"checkpoint.json", checkpoint); err != nil {
		return err
	}
	return seedStorageInventoryDue(ctx, client, bucket)
}

func seedStorageInventoryDue(ctx context.Context, client *s3.Client, bucket string) error {
	checkpoint, err := json.Marshal(seededMaintenanceCheckpoint{})
	if err != nil {
		return err
	}
	return putS3Object(ctx, client, bucket, diagnosticsS3Maintenance, checkpoint)
}

func seededUsageRunPrefix(prefix, snapshotID string, run seededUsageRun) string {
	return fmt.Sprintf("%ssnapshots/%s/usage/%s/%04d/%08d/", prefix, snapshotID, run.Stage, run.Level, run.Index)
}

func diagnosticsS3ObjectKey(storageKey string) (string, error) {
	relative, found := strings.CutPrefix(storageKey, diagnosticsStorageKeyPrefix)
	if !found {
		return "", fmt.Errorf("storage object key %q does not use prefix %q", storageKey, diagnosticsStorageKeyPrefix)
	}
	return diagnosticsS3ObjectPrefix + relative, nil
}

func storageSeedID(kind, value string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + value))
	return hex.EncodeToString(sum[:16])
}
