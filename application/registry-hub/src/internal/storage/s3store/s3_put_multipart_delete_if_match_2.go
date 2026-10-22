package s3store

import (
	"context"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"net/http"
	"registry-hub/internal/storage"
)

func (s *Store) DeleteIfMatch(ctx context.Context, key, version string) error {
	name, err := s.objectKey(key)
	if err != nil {
		return err
	}
	_, err = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(name), IfMatch: aws.String(version),
	})
	if err == nil {
		return nil
	}
	mapped := mapError(err)
	if errors.Is(mapped, storage.ErrConditionFailed) {
		return mapped
	}
	if shouldResolve(err) {
		entry, headErr := s.Head(ctx, key)
		if errors.Is(headErr, storage.ErrNotFound) {
			return nil
		}
		if headErr == nil && entry.Version != version {
			return storage.ErrConditionFailed
		}
		return errors.Join(storage.ErrUnknownCommit, mapped, mapError(headErr))
	}
	return mapped
}

func (s *Store) List(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	remotePrefix, err := s.objectKeyPrefix(prefix)
	if err != nil {
		return nil, "", err
	}
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	input := &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(remotePrefix), MaxKeys: aws.Int32(int32(limit))}
	if token != "" {
		input.ContinuationToken = aws.String(token)
	}
	result, err := s.client.ListObjectsV2(ctx, input)
	if err != nil {
		return nil, "", mapError(err)
	}
	entries := make([]storage.Entry, 0, len(result.Contents))
	for _, object := range result.Contents {
		remoteKey := aws.ToString(object.Key)
		logicalKey, ok := s.logicalKey(remoteKey)
		if !ok {
			continue
		}
		version := aws.ToString(object.ETag)
		if version == "" {
			return nil, "", errors.New("S3 list response did not provide an ETag")
		}
		entries = append(entries, storage.Entry{Key: logicalKey, Version: version, Size: aws.ToInt64(object.Size), ModTime: aws.ToTime(object.LastModified).UnixNano()})
	}
	return entries, aws.ToString(result.NextContinuationToken), nil
}

func (s *Store) Close() error {
	s.replayMu.Lock()
	s.replayClosed = true
	replay := s.replay
	s.replayMu.Unlock()
	var replayErr error
	if replay != nil {
		replayErr = replay.Close()
	}
	if s.httpClient != nil {
		if transport, ok := s.httpClient.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	}
	return replayErr
}
