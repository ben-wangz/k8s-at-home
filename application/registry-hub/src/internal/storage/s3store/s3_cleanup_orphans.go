package s3store

import (
	"context"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"hash"
	"io"
	"net"
	"net/http"
	"path"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

// CleanupOrphans aborts stale multipart uploads under the configured prefix.
func (s *Store) CleanupOrphans(ctx context.Context, olderThan time.Duration, limit int) (int, error) {
	if olderThan <= 0 {
		return 0, errors.New("orphan retention must be positive")
	}
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	cutoff := time.Now().Add(-olderThan)
	keyMarker := ""
	uploadMarker := ""
	removed := 0
	multipartPrefix := s.prefix
	if multipartPrefix != "" {
		multipartPrefix += "/"
	}
	for removed < limit {
		input := &s3.ListMultipartUploadsInput{
			Bucket: aws.String(s.bucket), Prefix: aws.String(multipartPrefix), MaxUploads: aws.Int32(int32(minInt(limit-removed, 1000))),
		}
		if keyMarker != "" {
			input.KeyMarker = aws.String(keyMarker)
		}
		if uploadMarker != "" {
			input.UploadIdMarker = aws.String(uploadMarker)
		}
		result, err := s.client.ListMultipartUploads(ctx, input)
		if err != nil {
			return removed, mapError(err)
		}
		for _, upload := range result.Uploads {
			if removed >= limit {
				break
			}
			if aws.ToTime(upload.Initiated).After(cutoff) || aws.ToString(upload.Key) == "" || aws.ToString(upload.UploadId) == "" {
				continue
			}
			_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
				Bucket: aws.String(s.bucket), Key: upload.Key, UploadId: upload.UploadId,
			})
			if err != nil {
				if shouldResolve(err) {
					continue
				}
				return removed, mapError(err)
			}
			removed++
		}
		if !aws.ToBool(result.IsTruncated) {
			break
		}
		keyMarker = aws.ToString(result.NextKeyMarker)
		uploadMarker = aws.ToString(result.NextUploadIdMarker)
		if keyMarker == "" && uploadMarker == "" {
			break
		}
	}
	return removed, nil
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func (s *Store) objectKey(key string) (string, error) {
	if !validKey(key) {
		return "", storage.ErrInvalidKey
	}
	if s.prefix == "" {
		return key, nil
	}
	return path.Join(s.prefix, key), nil
}

func (s *Store) objectKeyPrefix(key string) (string, error) {
	if key == "" {
		if s.prefix == "" {
			return "", nil
		}
		return s.prefix + "/", nil
	}
	trailingSlash := strings.HasSuffix(key, "/")
	key = strings.TrimSuffix(key, "/")
	if !validKey(key) {
		return "", storage.ErrInvalidKey
	}
	if s.prefix == "" {
		if trailingSlash {
			return key + "/", nil
		}
		return key, nil
	}
	joined := path.Join(s.prefix, key)
	if trailingSlash {
		joined += "/"
	}
	return joined, nil
}

func (s *Store) logicalKey(remoteKey string) (string, bool) {
	if s.prefix == "" {
		return remoteKey, validKey(remoteKey)
	}
	base := s.prefix + "/"
	if !strings.HasPrefix(remoteKey, base) {
		return "", false
	}
	key := strings.TrimPrefix(remoteKey, base)
	return key, validKey(key)
}

func validKey(key string) bool {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") {
		return false
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\x00\r\n") {
			return false
		}
	}
	return true
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(storage.ErrTimeout, err)
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		if networkError.Timeout() {
			return errors.Join(storage.ErrTimeout, err)
		}
		return errors.Join(storage.ErrUnavailable, err)
	}
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		switch apiError.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return storage.ErrNotFound
		case "PreconditionFailed", "ConditionalRequestConflict":
			return storage.ErrConditionFailed
		case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch":
			return storage.ErrPermissionDenied
		}
	}
	var responseError *smithyhttp.ResponseError
	if errors.As(err, &responseError) {
		status := responseError.HTTPStatusCode()
		if status >= http.StatusInternalServerError {
			return errors.Join(storage.ErrUnavailable, err)
		}
		switch status {
		case http.StatusNotFound:
			return storage.ErrNotFound
		case http.StatusPreconditionFailed, http.StatusConflict:
			return storage.ErrConditionFailed
		case http.StatusForbidden, http.StatusUnauthorized:
			return storage.ErrPermissionDenied
		case http.StatusRequestTimeout, http.StatusTooManyRequests:
			return storage.ErrUnavailable
		}
	}
	return fmt.Errorf("S3 operation: %w", err)
}

func shouldResolve(err error) bool {
	var responseError *smithyhttp.ResponseError
	if errors.As(err, &responseError) {
		return responseError.HTTPStatusCode() >= http.StatusInternalServerError
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return true
	}
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		return false
	}
	return err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

type digestReader struct {
	reader io.Reader
	hash   hash.Hash
	count  int64
}

func readerHasExtra(reader io.Reader) (bool, error) {
	var one [1]byte
	n, err := reader.Read(one[:])
	if n > 0 {
		return true, nil
	}
	if errors.Is(err, io.EOF) {
		return false, nil
	}
	return false, err
}

func (r *digestReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	if n > 0 {
		_, _ = r.hash.Write(buffer[:n])
		r.count += int64(n)
	}
	return n, err
}
