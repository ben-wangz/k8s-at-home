package s3

import (
	"bytes"
	"context"
	"io"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"git-repo-backup/internal/observability"
)

// applySSE sets the configured server-side encryption on a PutObject input.
func (s *Store) applySSE(in *s3.PutObjectInput) {
	switch s.cfg.ServerSideEncrypt {
	case "AES256":
		in.ServerSideEncryption = types.ServerSideEncryptionAes256
	case "aws:kms":
		in.ServerSideEncryption = types.ServerSideEncryptionAwsKms
		in.SSEKMSKeyId = &s.cfg.KMSKeyID
	}
}

// applySSECreate sets server-side encryption on a multipart create input.
func (s *Store) applySSECreate(in *s3.CreateMultipartUploadInput) {
	switch s.cfg.ServerSideEncrypt {
	case "AES256":
		in.ServerSideEncryption = types.ServerSideEncryptionAes256
	case "aws:kms":
		in.ServerSideEncryption = types.ServerSideEncryptionAwsKms
		in.SSEKMSKeyId = &s.cfg.KMSKeyID
	}
}

// putIfAbsent writes body to key only when the object does not exist.
func (s *Store) putIfAbsent(ctx context.Context, key string, body []byte, contentType string) error {
	reqCtx, cancel := withTimeout(ctx)
	defer cancel()
	in := &s3.PutObjectInput{
		Bucket:        &s.bucket,
		Key:           &key,
		Body:          bytes.NewReader(body),
		ContentLength: int64Ptr(int64(len(body))),
		IfNoneMatch:   strPtr("*"),
	}
	if contentType != "" {
		in.ContentType = &contentType
	}
	s.applySSE(in)
	_, err := s.client.PutObject(reqCtx, in)
	if err != nil {
		if isPreconditionFailed(err) {
			return errExists
		}
		return observability.WrapSafe(observability.CodeStorageFailed,
			"conditional put failed", err)
	}
	return nil
}

// getSmallObject reads an object expected to be small.
func (s *Store) getSmallObject(ctx context.Context, key string) ([]byte, error) {
	reqCtx, cancel := withTimeout(ctx)
	defer cancel()
	out, err := s.client.GetObject(reqCtx, &s3.GetObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
	})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	data, err := io.ReadAll(io.LimitReader(out.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return data, nil
}

// headObject fetches object metadata without downloading content.
func (s *Store) headObject(ctx context.Context, key string) (*s3.HeadObjectOutput, error) {
	reqCtx, cancel := withTimeout(ctx)
	defer cancel()
	return s.client.HeadObject(reqCtx, &s3.HeadObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
	})
}

// prefixIsEmpty reports whether no object exists under prefix.
func (s *Store) prefixIsEmpty(ctx context.Context, prefix string) (bool, error) {
	reqCtx, cancel := withTimeout(ctx)
	defer cancel()
	out, err := s.client.ListObjectsV2(reqCtx, &s3.ListObjectsV2Input{
		Bucket:  &s.bucket,
		Prefix:  &prefix,
		MaxKeys: int32Ptr(1),
	})
	if err != nil {
		return false, observability.WrapSafe(observability.CodeStorageFailed,
			"list destination prefix", err)
	}
	return len(out.Contents) == 0, nil
}

func strPtr(s string) *string { return &s }
func int32Ptr(i int32) *int32 { return &i }
func int64Ptr(i int64) *int64 { return &i }
