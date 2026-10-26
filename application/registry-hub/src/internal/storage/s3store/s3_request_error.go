package s3store

type s3RequestError struct {
	requestID string
	cause     error
}

func (e *s3RequestError) Error() string       { return e.cause.Error() }
func (e *s3RequestError) Unwrap() error       { return e.cause }
func (e *s3RequestError) S3RequestID() string { return e.requestID }
