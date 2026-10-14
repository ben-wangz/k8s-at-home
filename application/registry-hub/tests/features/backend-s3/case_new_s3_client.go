package backends3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

func newS3Client(ctx context.Context, endpoint, accessKey, secretKey string) (*s3.Client, error) {
	awsConfig, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
		awsconfig.WithHTTPClient(&http.Client{Timeout: 20 * time.Second}),
	)
	if err != nil {
		return nil, err
	}
	return s3.NewFromConfig(awsConfig, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(strings.TrimSuffix(endpoint, "/"))
		options.UsePathStyle = true
	}), nil
}

func s3ServiceEndpoint(ctx context.Context, environment testkit.CaseContext, name string) (string, error) {
	service, err := testkit.KubectlGet(ctx, environment.Namespace, "service", name)
	if err != nil {
		return "", err
	}
	spec := testkit.Map(service["spec"])
	clusterIP := testkit.String(spec["clusterIP"])
	if clusterIP == "" || clusterIP == "None" {
		return "", fmt.Errorf("backend S3 fixture Service has no ClusterIP")
	}
	for _, raw := range testkit.Slice(spec["ports"]) {
		port := testkit.Map(raw)
		if testkit.String(port["name"]) == "s3" && testkit.Int(port["port"]) == 8080 {
			return fmt.Sprintf("http://%s:8080", clusterIP), nil
		}
	}
	return "", fmt.Errorf("backend S3 fixture Service has no S3 port")
}

func rejectWrongCredentials(ctx context.Context, endpoint, bucket string) error {
	client, err := newS3Client(ctx, endpoint, "invalid-access-key", "invalid-secret-key")
	if err != nil {
		return err
	}
	_, err = client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), MaxKeys: aws.Int32(1)})
	if err == nil {
		return fmt.Errorf("S3 fixture accepted invalid credentials")
	}
	return nil
}

func listBackendObjects(ctx context.Context, client *s3.Client, bucket string) (map[string]string, error) {
	input := &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(backendS3Prefix + "/"), MaxKeys: aws.Int32(500)}
	paginator := s3.NewListObjectsV2Paginator(client, input)
	objects := make(map[string]string)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list backend S3 state objects: %w", err)
		}
		for _, object := range page.Contents {
			key := aws.ToString(object.Key)
			if !strings.HasPrefix(key, backendS3Prefix+"/") {
				return nil, fmt.Errorf("S3 object escaped the configured backend prefix")
			}
			objects[key] = fmt.Sprintf("%s:%d", aws.ToString(object.ETag), aws.ToInt64(object.Size))
		}
	}
	return objects, nil
}

func getTask(ctx context.Context, environment testkit.CaseContext, taskID string) (map[string]any, error) {
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(taskID), nil, nil)
	if err != nil {
		return nil, err
	}
	var task map[string]any
	if _, err := support.DecodeJSON(response, 2<<20, &task); err != nil {
		return nil, err
	}
	return task, nil
}

func solidifiedTask(task map[string]any, taskID, digest string) bool {
	return testkit.String(task["id"]) == taskID && testkit.String(task["status"]) == "solidified" &&
		testkit.String(task["rootDigest"]) == digest && task["rootPublished"] == true
}

func compareObjectSnapshots(before, after map[string]string) error {
	for key, value := range before {
		if strings.HasSuffix(key, "/representation-index.json") {
			continue
		}
		if after[key] != value {
			return fmt.Errorf("backend S3 object %s changed or disappeared across restart", key)
		}
	}
	return nil
}

func snapshotDigest(objects map[string]string) string {
	keys := make([]string, 0, len(objects))
	for key := range objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	hash := sha256.New()
	for _, key := range keys {
		_, _ = fmt.Fprintf(hash, "%s\x00%s\n", key, objects[key])
	}
	return hex.EncodeToString(hash.Sum(nil))
}
