package s3store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"io"
	"net/http"
	"registry-hub/internal/storage"
)

func (s *Store) putMultipart(ctx context.Context, key, name string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	created, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(name),
	})
	if err != nil {
		return storage.Entry{}, mapError(err)
	}
	uploadID := aws.ToString(created.UploadId)
	if uploadID == "" {
		return storage.Entry{}, errors.New("S3 multipart upload did not return an upload ID")
	}
	abort := func() error {
		_, abortErr := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket: aws.String(s.bucket), Key: aws.String(name), UploadId: aws.String(uploadID),
		})
		return abortErr
	}
	source := &digestReader{reader: body, hash: sha256.New()}
	parts := make([]types.CompletedPart, 0, int((size+s.partSize-1)/s.partSize))
	remaining := size
	for partNumber := int32(1); remaining > 0; partNumber++ {
		if partNumber > 10000 {
			_ = abort()
			return storage.Entry{}, errors.New("S3 multipart upload exceeds 10000 parts")
		}
		partSize := s.partSize
		if remaining < partSize {
			partSize = remaining
		}
		uploaded, uploadErr := s.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String(s.bucket), Key: aws.String(name), UploadId: aws.String(uploadID),
			PartNumber: aws.Int32(partNumber), Body: io.LimitReader(source, partSize), ContentLength: aws.Int64(partSize),
		}, func(options *s3.Options) {
			options.APIOptions = append(options.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
		})
		if uploadErr != nil {
			return storage.Entry{}, errors.Join(mapError(uploadErr), abort())
		}
		if aws.ToString(uploaded.ETag) == "" {
			return storage.Entry{}, errors.Join(errors.New("S3 UploadPart response did not provide an ETag"), abort())
		}
		parts = append(parts, types.CompletedPart{ETag: uploaded.ETag, PartNumber: aws.Int32(partNumber)})
		remaining -= partSize
	}
	if source.count != size {
		return storage.Entry{}, errors.Join(errors.New("S3 multipart stream length mismatch"), abort())
	}
	if extra, extraErr := readerHasExtra(source); extra || extraErr != nil {
		if extraErr != nil {
			return storage.Entry{}, errors.Join(fmt.Errorf("S3 multipart stream could not be verified: %w", extraErr), abort())
		}
		return storage.Entry{}, errors.Join(errors.New("S3 multipart stream length mismatch"), abort())
	}
	complete := &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(s.bucket), Key: aws.String(name), UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	}
	applyConditions(&complete.IfNoneMatch, &complete.IfMatch, options)
	result, err := s.client.CompleteMultipartUpload(ctx, complete)
	if err != nil {
		mapped := mapPutError(err, options)
		if shouldResolve(err) {
			if entry, ok := s.resolvePut(ctx, key, size, source.hash.Sum(nil)); ok {
				return entry, nil
			}
			return storage.Entry{}, errors.Join(storage.ErrUnknownCommit, mapped, abort())
		}
		return storage.Entry{}, errors.Join(mapped, abort())
	}
	version := aws.ToString(result.ETag)
	if version == "" {
		if entry, ok := s.resolvePut(ctx, key, size, source.hash.Sum(nil)); ok {
			return entry, nil
		}
		return storage.Entry{}, errors.Join(storage.ErrUnknownCommit, errors.New("S3 CompleteMultipartUpload response did not provide an ETag"))
	}
	return storage.Entry{Key: key, Version: version, Size: size}, nil
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

func (s *Store) resolvePut(ctx context.Context, key string, size int64, expected []byte) (storage.Entry, bool) {
	entry, err := s.Head(ctx, key)
	if err != nil || entry.Size != size {
		return storage.Entry{}, false
	}
	object, err := s.Get(ctx, key)
	if err != nil {
		return storage.Entry{}, false
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, object.Body)
	closeErr := object.Body.Close()
	if copyErr != nil || closeErr != nil {
		return storage.Entry{}, false
	}
	if !bytes.Equal(hash.Sum(nil), expected) {
		return storage.Entry{}, false
	}
	return entry, true
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
	if transport, ok := s.httpClient.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
	return nil
}
