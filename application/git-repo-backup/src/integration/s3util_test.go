//go:build integration

// SDK helpers for verifying fixture state independently of the backup binary.
package integration

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func listAll(t *testing.T, client *s3.Client, bucket, prefix string) []string {
	t.Helper()
	var out []string
	pag := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: &bucket, Prefix: &prefix,
	})
	for pag.HasMorePages() {
		page, err := pag.NextPage(context.Background())
		if err != nil {
			t.Fatalf("list %s: %v", prefix, err)
		}
		for _, obj := range page.Contents {
			if obj.Key != nil {
				out = append(out, *obj.Key)
			}
		}
	}
	return out
}

func headOK(t *testing.T, client *s3.Client, bucket, key string) error {
	t.Helper()
	_, err := client.HeadObject(context.Background(), &s3.HeadObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	return err
}

func getObject(t *testing.T, client *s3.Client, bucket, key string) []byte {
	t.Helper()
	out, err := client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer out.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := out.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf
}
