package environment

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"strings"
	"time"
)

func backendS3Pod(name, runID, fullname, secretName string) map[string]any {
	labels := fixtureLabels(runID, fullname, name)
	return map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": Namespace, "labels": labels},
		"spec": map[string]any{
			"automountServiceAccountToken": false,
			"restartPolicy":                "Always",
			"securityContext": map[string]any{
				"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532, "fsGroup": 65532,
				"seccompProfile": map[string]any{"type": "RuntimeDefault"},
			},
			"containers": []map[string]any{{
				"name": "versitygw", "image": backendS3Image, "imagePullPolicy": "IfNotPresent",
				"args":  []string{"--access-log", "stdout", "--quiet", "--port", ":8080", "--region", "us-east-1", "posix", "/data"},
				"ports": []map[string]any{{"name": "s3", "containerPort": 8080, "protocol": "TCP"}},
				"env": []map[string]any{
					{"name": "HOME", "value": "/tmp"},
					{"name": "ROOT_ACCESS_KEY_ID", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": secretName, "key": "AWS_ACCESS_KEY_ID"}}},
					{"name": "ROOT_SECRET_ACCESS_KEY", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": secretName, "key": "AWS_SECRET_ACCESS_KEY"}}},
				},
				"readinessProbe": map[string]any{
					"tcpSocket":     map[string]any{"port": "s3"},
					"periodSeconds": 2, "timeoutSeconds": 3, "failureThreshold": 60,
				},
				"livenessProbe": map[string]any{
					"tcpSocket":     map[string]any{"port": "s3"},
					"periodSeconds": 10, "timeoutSeconds": 3, "failureThreshold": 6,
				},
				"resources": map[string]any{
					"requests": map[string]string{"cpu": "100m", "memory": "256Mi"},
					"limits":   map[string]string{"cpu": "1", "memory": "1Gi"},
				},
				"securityContext": map[string]any{
					"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
					"capabilities": map[string]any{"drop": []string{"ALL"}},
				},
				"volumeMounts": []map[string]any{{"name": "data", "mountPath": "/data"}, {"name": "tmp", "mountPath": "/tmp"}},
			}},
			"volumes": []map[string]any{{"name": "data", "emptyDir": map[string]any{"sizeLimit": "1Gi"}}, {"name": "tmp", "emptyDir": map[string]any{}}},
		},
	}
}

func fixtureLabels(runID, fullname, fixtureName string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/instance": strings.TrimSuffix(fullname, "-registry-hub"),
		"registry-hub.io/test-id":    runID,
		"registry-hub.io/instance":   fullname,
		"registry-hub.io/managed-by": "registry-hub-test-fixture",
		"registry-hub.io/fixture":    fixtureName,
	}
}

func validateFixtureOwnership(object map[string]any, name, runID, fullname string) error {
	metadata := testkit.Map(object["metadata"])
	labels := testkit.Map(metadata["labels"])
	if testkit.String(metadata["name"]) != name || testkit.String(metadata["namespace"]) != Namespace ||
		testkit.String(metadata["uid"]) == "" || testkit.String(labels["registry-hub.io/test-id"]) != runID ||
		testkit.String(labels["registry-hub.io/instance"]) != fullname ||
		testkit.String(labels["registry-hub.io/managed-by"]) != "registry-hub-test-fixture" ||
		testkit.String(labels["registry-hub.io/fixture"]) != name {
		return fmt.Errorf("backend S3 fixture %s has unexpected ownership", name)
	}
	return nil
}

func createFixture(ctx context.Context, payload map[string]any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := testkit.CommandInput(ctx, encoded, "kubectl", "create", "-f", "-"); err != nil {
		return fmt.Errorf("create backend S3 fixture: %w", err)
	}
	return nil
}

func ensureS3Bucket(ctx context.Context, endpoint, bucket, accessKey, secretKey string) error {
	client, err := newFixtureS3Client(ctx, endpoint, accessKey, secretKey)
	if err != nil {
		return err
	}
	if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err == nil {
		return nil
	}
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		if _, headErr := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); headErr == nil {
			return nil
		}
		return fmt.Errorf("create backend S3 bucket: %w", err)
	}
	return nil
}

func newFixtureS3Client(ctx context.Context, endpoint, accessKey, secretKey string) (*s3.Client, error) {
	awsConfig, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
		awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		awsconfig.WithHTTPClient(&http.Client{Timeout: 15 * time.Second}),
	)
	if err != nil {
		return nil, err
	}
	return s3.NewFromConfig(awsConfig, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(strings.TrimSuffix(endpoint, "/"))
		options.UsePathStyle = true
	}), nil
}

func verifyS3ConditionalOperations(ctx context.Context, endpoint, bucket, accessKey, secretKey string) error {
	client, err := newFixtureS3Client(ctx, endpoint, accessKey, secretKey)
	if err != nil {
		return err
	}
	keySuffix, err := randomS3Credential()
	if err != nil {
		return err
	}
	key := "registry-hub-fixture-probes/" + keySuffix
	defer func() {
		_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	}()

	created, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader("first"), IfNoneMatch: aws.String("*"),
	})
	if err != nil {
		return fmt.Errorf("S3 fixture conditional create failed: %s", s3ErrorClass(err))
	}
	if aws.ToString(created.ETag) == "" {
		return fmt.Errorf("S3 fixture conditional create returned no ETag")
	}
	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader("duplicate"), IfNoneMatch: aws.String("*"),
	})
	if !isS3PreconditionFailure(err) {
		return fmt.Errorf("S3 fixture conditional create did not reject a duplicate: %s", s3ErrorClass(err))
	}
	updated, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader("updated"), IfMatch: created.ETag,
	})
	if err != nil {
		return fmt.Errorf("S3 fixture conditional update failed: %s", s3ErrorClass(err))
	}
	if aws.ToString(updated.ETag) == "" {
		return fmt.Errorf("S3 fixture conditional update returned no ETag")
	}
	_, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), IfMatch: created.ETag})
	if !isS3PreconditionFailure(err) {
		return fmt.Errorf("S3 fixture conditional delete did not reject a stale ETag: %s", s3ErrorClass(err))
	}
	if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), IfMatch: updated.ETag}); err != nil {
		return fmt.Errorf("S3 fixture conditional delete failed: %s", s3ErrorClass(err))
	}
	return nil
}

func isS3PreconditionFailure(err error) bool {
	if err == nil {
		return false
	}
	var apiError smithy.APIError
	if errors.As(err, &apiError) && (apiError.ErrorCode() == "PreconditionFailed" || apiError.ErrorCode() == "ConditionalRequestConflict") {
		return true
	}
	var responseError *smithyhttp.ResponseError
	if errors.As(err, &responseError) {
		return responseError.HTTPStatusCode() == http.StatusPreconditionFailed || responseError.HTTPStatusCode() == http.StatusConflict
	}
	return false
}

func s3ErrorClass(err error) string {
	if err == nil {
		return "success"
	}
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		return apiError.ErrorCode()
	}
	var responseError *smithyhttp.ResponseError
	if errors.As(err, &responseError) {
		return fmt.Sprintf("HTTP %d", responseError.HTTPStatusCode())
	}
	return fmt.Sprintf("%T", err)
}

func validateBackendS3Credentials(data map[string][]byte) error {
	if len(data) != 2 || len(data["AWS_ACCESS_KEY_ID"]) < 32 || len(data["AWS_SECRET_ACCESS_KEY"]) < 32 {
		return fmt.Errorf("S3 credential Secret has an unexpected shape")
	}
	return nil
}

func randomS3Credential() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func digestS3Credentials(data map[string][]byte) string {
	encoded, _ := json.Marshal(data)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
