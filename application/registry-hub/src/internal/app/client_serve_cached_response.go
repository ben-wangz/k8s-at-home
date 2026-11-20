package app

import "net/http"

func recordBlobResponseOutcome(request *http.Request, written, expected int64, copyErr error) {
	meta, _ := request.Context().Value(requestMetaKey{}).(*requestMeta)
	if meta == nil {
		return
	}
	meta.responseBodyBytes = written
	switch {
	case copyErr != nil:
		meta.responseBodyState = "copy_error"
		meta.failureStage = "blob_response_copy"
		meta.failureCause = "copy_error"
	case expected >= 0 && written != expected:
		meta.responseBodyState = "length_mismatch"
		meta.failureStage = "blob_response_copy"
		meta.failureCause = "length_mismatch"
	default:
		meta.responseBodyState = "copy_complete"
	}
}
