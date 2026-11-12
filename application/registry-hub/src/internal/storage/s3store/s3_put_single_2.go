package s3store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"os"
	"registry-hub/internal/storage"
)

func (s *Store) putSingle(ctx context.Context, key, name string, source *replaySource, options storage.PutOptions) (storage.Entry, error) {
	operationID, err := newOperationID()
	if err != nil {
		return storage.Entry{}, err
	}
	body, err := source.OpenRange(0, source.size)
	if err != nil {
		return storage.Entry{}, err
	}
	defer body.Close()
	ctx, retryObserver := withRetryObserver(ctx)
	input := &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(name),
		Body:          body,
		ContentLength: aws.Int64(source.size),
		Metadata:      map[string]string{"registry-hub-operation": operationID},
	}
	checksum := base64.StdEncoding.EncodeToString(source.digest[:])
	if s.checksumSHA256 {
		input.ChecksumSHA256 = aws.String(checksum)
	}
	applyConditions(&input.IfNoneMatch, &input.IfMatch, options)
	result, err := s.client.PutObject(ctx, input, func(options *s3.Options) {
		options.APIOptions = append(options.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
	})
	if err != nil {
		mapped := mapPutError(err, options)
		if errors.Is(mapped, storage.ErrConditionFailed) && !retryObserver.retried.Load() {
			return storage.Entry{}, mapped
		}
		if shouldResolve(err) || retryObserver.retried.Load() {
			entry, ok, readErr := s.resolvePut(ctx, key, source.size, source.digest[:], operationID)
			if ok {
				return entry, nil
			}
			return storage.Entry{}, errors.Join(storage.ErrUnknownCommit, mapped, readErr)
		}
		return storage.Entry{}, mapped
	}
	version := aws.ToString(result.ETag)
	if version == "" {
		entry, ok, readErr := s.resolvePut(ctx, key, source.size, source.digest[:], operationID)
		if ok {
			return entry, nil
		}
		return storage.Entry{}, errors.Join(storage.ErrUnknownCommit, errors.New("S3 PutObject response did not provide an ETag"), readErr)
	}
	entry := storage.Entry{Key: key, Version: version, Size: source.size}
	if s.checksumSHA256 && aws.ToString(result.ChecksumSHA256) == checksum {
		entry.ChecksumAlgorithm, entry.ChecksumValue, entry.ChecksumScope = "sha256", checksum, "complete_object"
	} else if s.checksumSHA256 && result.ChecksumSHA256 != nil {
		resolved, ok, readErr := s.resolvePut(ctx, key, source.size, source.digest[:], operationID)
		if ok {
			return resolved, nil
		}
		return storage.Entry{}, errors.Join(storage.ErrUnknownCommit, storage.ErrCorruptObject, readErr)
	}
	return entry, nil
}

func (s *Store) ensureReplayManager() (*replayManager, error) {
	s.replayMu.Lock()
	defer s.replayMu.Unlock()
	if s.replayClosed {
		return nil, errors.New("S3 store is closed")
	}
	if s.replay != nil {
		return s.replay, nil
	}
	directory, err := os.MkdirTemp("", "registry-hub-s3-replay-")
	if err != nil {
		return nil, fmt.Errorf("create private S3 replay directory: %w", err)
	}
	replay, err := newReplayManager(replayConfig{directory: directory})
	if err != nil {
		_ = os.RemoveAll(directory)
		return nil, err
	}
	replay.removeDir = true
	s.replay = replay
	return replay, nil
}
