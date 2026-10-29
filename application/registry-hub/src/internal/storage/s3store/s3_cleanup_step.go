package s3store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"registry-hub/internal/storage"
	"time"
)

func (s *Store) CleanupOrphansStep(ctx context.Context, olderThan time.Duration, token string) (string, int, error) {
	var marker struct {
		Key    string `json:"key"`
		Upload string `json:"upload"`
	}
	if token != "" && json.Unmarshal([]byte(token), &marker) != nil {
		return token, 0, storage.ErrCorruptObject
	}
	prefix := s.prefix
	if prefix != "" {
		prefix += "/"
	}
	input := &s3.ListMultipartUploadsInput{Bucket: aws.String(s.bucket), Prefix: aws.String(prefix), MaxUploads: aws.Int32(16)}
	if marker.Key != "" {
		input.KeyMarker = aws.String(marker.Key)
	}
	if marker.Upload != "" {
		input.UploadIdMarker = aws.String(marker.Upload)
	}
	result, err := s.client.ListMultipartUploads(ctx, input)
	if err != nil {
		return token, 0, mapError(err)
	}
	removed := 0
	var failure error
	for _, upload := range result.Uploads {
		key := aws.ToString(upload.Key)
		if key == "" || aws.ToString(upload.UploadId) == "" || aws.ToTime(upload.Initiated).After(time.Now().Add(-olderThan)) {
			continue
		}
		release := s.claimMultipartCleanup(key)
		if release == nil {
			continue
		}
		_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(s.bucket), Key: upload.Key, UploadId: upload.UploadId})
		release()
		var apiError smithy.APIError
		if errors.As(err, &apiError) && apiError.ErrorCode() == "NoSuchUpload" {
			err = nil
		}
		if err != nil {
			failure = errors.Join(failure, mapError(err))
		} else {
			removed++
		}
	}
	marker.Key, marker.Upload = aws.ToString(result.NextKeyMarker), aws.ToString(result.NextUploadIdMarker)
	if !aws.ToBool(result.IsTruncated) {
		return "", removed, failure
	}
	data, _ := json.Marshal(marker)
	return string(data), removed, failure
}
