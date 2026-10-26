package s3store

import (
	"errors"
	"net/http"
	"testing"

	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"registry-hub/internal/storage"
)

func TestMapErrorPreservesS3RequestIDAndStorageClassification(t *testing.T) {
	requestErr := &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     http.Header{"X-Amz-Request-Id": []string{"rustfs-request-123"}},
		}},
		Err: &smithy.GenericAPIError{Code: "ServiceUnavailable", Message: "temporary storage failure"},
	}
	err := mapError(requestErr)
	if !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("mapped error = %v, want storage unavailable", err)
	}
	var identified interface{ S3RequestID() string }
	if !errors.As(err, &identified) || identified.S3RequestID() != "rustfs-request-123" {
		t.Fatalf("mapped error request ID = %v, want rustfs-request-123", identified)
	}
}
