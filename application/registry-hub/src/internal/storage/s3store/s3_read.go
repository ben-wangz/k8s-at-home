package s3store

import (
	"context"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"net/http"
	"registry-hub/internal/storage"
)

func (s *Store) Get(ctx context.Context, key string) (storage.Object, error) {
	return s.get(ctx, key, "")
}

func (s *Store) GetIfMatch(ctx context.Context, key, version string) (storage.Object, error) {
	return s.get(ctx, key, version)
}

func (s *Store) get(ctx context.Context, key, expected string) (storage.Object, error) {
	name, err := s.objectKey(key)
	if err != nil {
		return storage.Object{}, err
	}
	input := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(name)}
	if s.checksumSHA256 {
		input.ChecksumMode = types.ChecksumModeEnabled
	}
	if expected != "" {
		input.IfMatch = aws.String(expected)
	}
	result, err := s.client.GetObject(ctx, input)
	if err != nil && input.ChecksumMode != "" && checksumReadbackCanFallback(err) {
		input.ChecksumMode = ""
		result, err = s.client.GetObject(ctx, input)
	}
	if err != nil {
		return storage.Object{}, mapError(err)
	}
	version := aws.ToString(result.ETag)
	if version == "" {
		_ = result.Body.Close()
		return storage.Object{}, errors.New("S3 object did not provide an ETag for conditional operations")
	}
	object := storage.Object{Body: result.Body, Size: aws.ToInt64(result.ContentLength), Version: version, ModTime: aws.ToTime(result.LastModified).UnixNano()}
	if result.ChecksumSHA256 != nil {
		object.ChecksumAlgorithm, object.ChecksumValue, object.ChecksumScope = "sha256", aws.ToString(result.ChecksumSHA256), "complete_object"
	}
	return object, nil
}

func (s *Store) Head(ctx context.Context, key string) (storage.Entry, error) {
	name, err := s.objectKey(key)
	if err != nil {
		return storage.Entry{}, err
	}
	input := &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(name)}
	if s.checksumSHA256 {
		input.ChecksumMode = types.ChecksumModeEnabled
	}
	result, err := s.client.HeadObject(ctx, input)
	if err != nil && input.ChecksumMode != "" && checksumReadbackCanFallback(err) {
		input.ChecksumMode = ""
		result, err = s.client.HeadObject(ctx, input)
	}
	if err != nil {
		return storage.Entry{}, mapError(err)
	}
	version := aws.ToString(result.ETag)
	if version == "" {
		return storage.Entry{}, errors.New("S3 object did not provide an ETag for conditional operations")
	}
	entry := storage.Entry{Key: key, Version: version, Size: aws.ToInt64(result.ContentLength), ModTime: aws.ToTime(result.LastModified).UnixNano()}
	if result.ChecksumSHA256 != nil {
		entry.ChecksumAlgorithm, entry.ChecksumValue, entry.ChecksumScope = "sha256", aws.ToString(result.ChecksumSHA256), "complete_object"
	}
	return entry, nil
}

func checksumReadbackCanFallback(err error) bool {
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		switch apiError.ErrorCode() {
		case "AccessDenied", "KMS.AccessDeniedException", "InvalidRequest", "NotImplemented", "NotSupported":
			return true
		}
	}
	var responseError interface{ HTTPStatusCode() int }
	return errors.As(err, &responseError) && responseError.HTTPStatusCode() == http.StatusForbidden
}
