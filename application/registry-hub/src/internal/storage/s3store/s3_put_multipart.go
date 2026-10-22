package s3store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"io"
	"registry-hub/internal/storage"
	"time"
)

func (s *Store) putMultipart(ctx context.Context, key, name string, source *replaySource, options storage.PutOptions) (storage.Entry, error) {
	partSizeLimit := s.partSize
	if partSizeLimit <= 0 {
		partSizeLimit = 16 << 20
	}
	partCount := (source.size-1)/partSizeLimit + 1
	if partCount > 10000 || partCount > int64(maxInt()) {
		return storage.Entry{}, errors.New("S3 multipart upload exceeds the supported part count")
	}
	operationID, err := newOperationID()
	if err != nil {
		return storage.Entry{}, err
	}
	releaseOwnership, err := s.beginMultipart(ctx, name)
	if err != nil {
		return storage.Entry{}, err
	}
	defer releaseOwnership()
	created, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(name),
		Metadata: map[string]string{"registry-hub-operation": operationID},
	})
	if err != nil {
		return storage.Entry{}, mapError(err)
	}
	uploadID := aws.ToString(created.UploadId)
	if uploadID == "" {
		return storage.Entry{}, errors.New("S3 multipart upload did not return an upload ID")
	}
	abort := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, abortErr := s.client.AbortMultipartUpload(cleanupCtx, &s3.AbortMultipartUploadInput{
			Bucket: aws.String(s.bucket), Key: aws.String(name), UploadId: aws.String(uploadID),
		})
		return abortErr
	}
	parts := make([]types.CompletedPart, 0, int(partCount))
	remaining := source.size
	var offset int64
	for partNumber := int32(1); remaining > 0; partNumber++ {
		if partNumber > 10000 {
			_ = abort()
			return storage.Entry{}, errors.New("S3 multipart upload exceeds 10000 parts")
		}
		partSize := partSizeLimit
		if remaining < partSize {
			partSize = remaining
		}
		partBody, openErr := source.OpenRange(offset, partSize)
		if openErr != nil {
			return storage.Entry{}, errors.Join(openErr, abort())
		}
		uploaded, uploadErr := s.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String(s.bucket), Key: aws.String(name), UploadId: aws.String(uploadID),
			PartNumber: aws.Int32(partNumber), Body: partBody, ContentLength: aws.Int64(partSize),
		}, func(options *s3.Options) {
			options.APIOptions = append(options.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
		})
		_ = partBody.Close()
		if uploadErr != nil {
			return storage.Entry{}, errors.Join(mapError(uploadErr), abort())
		}
		if aws.ToString(uploaded.ETag) == "" {
			return storage.Entry{}, errors.Join(errors.New("S3 UploadPart response did not provide an ETag"), abort())
		}
		parts = append(parts, types.CompletedPart{ETag: uploaded.ETag, PartNumber: aws.Int32(partNumber)})
		remaining -= partSize
		offset += partSize
	}
	complete := &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(s.bucket), Key: aws.String(name), UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	}
	applyConditions(&complete.IfNoneMatch, &complete.IfMatch, options)
	ctx, retryObserver := withRetryObserver(ctx)
	result, err := s.client.CompleteMultipartUpload(ctx, complete)
	if err != nil {
		mapped := mapPutError(err, options)
		if errors.Is(mapped, storage.ErrConditionFailed) && !retryObserver.retried.Load() {
			return storage.Entry{}, errors.Join(mapped, abort())
		}
		if shouldResolve(err) || retryObserver.retried.Load() {
			entry, ok, readErr := s.resolvePut(ctx, key, source.size, source.digest[:], operationID)
			if ok {
				return entry, nil
			}
			return storage.Entry{}, errors.Join(storage.ErrUnknownCommit, mapped, readErr)
		}
		return storage.Entry{}, errors.Join(mapped, abort())
	}
	version := aws.ToString(result.ETag)
	if version == "" {
		entry, ok, readErr := s.resolvePut(ctx, key, source.size, source.digest[:], operationID)
		if ok {
			return entry, nil
		}
		return storage.Entry{}, errors.Join(storage.ErrUnknownCommit, errors.New("S3 CompleteMultipartUpload response did not provide an ETag"), readErr)
	}
	return storage.Entry{Key: key, Version: version, Size: source.size}, nil
}

func applyConditions(ifNoneMatch, ifMatch **string, options storage.PutOptions) {
	if options.IfAbsent {
		*ifNoneMatch = aws.String("*")
	}
	if options.IfMatch != "" {
		*ifMatch = aws.String(options.IfMatch)
	}
}

func mapPutError(err error, options storage.PutOptions) error {
	mapped := mapError(err)
	if options.IfAbsent && errors.Is(mapped, storage.ErrConditionFailed) {
		return storage.ErrAlreadyExists
	}
	return mapped
}

func (s *Store) resolvePut(ctx context.Context, key string, size int64, expected []byte, operationID string) (storage.Entry, bool, error) {
	name, err := s.objectKey(key)
	if err != nil {
		return storage.Entry{}, false, err
	}
	head, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(name)})
	if err != nil {
		return storage.Entry{}, false, mapError(err)
	}
	if aws.ToInt64(head.ContentLength) != size || head.Metadata["registry-hub-operation"] != operationID || aws.ToString(head.ETag) == "" {
		return storage.Entry{}, false, nil
	}
	object, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(name), IfMatch: head.ETag, VersionId: head.VersionId,
	})
	if err != nil {
		return storage.Entry{}, false, mapError(err)
	}
	if object.Metadata["registry-hub-operation"] != operationID || aws.ToString(object.ETag) != aws.ToString(head.ETag) ||
		aws.ToString(object.VersionId) != aws.ToString(head.VersionId) || aws.ToInt64(object.ContentLength) != size {
		if closeErr := object.Body.Close(); closeErr != nil {
			return storage.Entry{}, false, mapError(closeErr)
		}
		return storage.Entry{}, false, nil
	}
	hash := sha256.New()
	reader := io.Reader(object.Body)
	if size < int64(^uint64(0)>>1) {
		reader = io.LimitReader(object.Body, size+1)
	}
	count, copyErr := io.Copy(hash, reader)
	closeErr := object.Body.Close()
	if copyErr != nil || closeErr != nil {
		return storage.Entry{}, false, errors.Join(mapError(copyErr), mapError(closeErr))
	}
	if count != size || !bytes.Equal(hash.Sum(nil), expected) {
		return storage.Entry{}, false, nil
	}
	entry := storage.Entry{Key: key, Version: aws.ToString(head.ETag), Size: size}
	return entry, true, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	name, err := s.objectKey(key)
	if err != nil {
		return err
	}
	_, err = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(name)})
	if err == nil {
		return nil
	}
	if shouldResolve(err) {
		_, headErr := s.Head(ctx, key)
		if errors.Is(headErr, storage.ErrNotFound) {
			return nil
		}
		return errors.Join(storage.ErrUnknownCommit, mapError(err), mapError(headErr))
	}
	return mapError(err)
}
