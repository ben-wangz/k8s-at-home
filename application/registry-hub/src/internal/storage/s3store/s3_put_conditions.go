package s3store

import (
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"registry-hub/internal/storage"
)

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
