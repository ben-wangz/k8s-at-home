package artifact

import "errors"

type cacheReadFailure struct {
	stage string
	err   error
}

func (e *cacheReadFailure) Error() string { return e.err.Error() }

func (e *cacheReadFailure) Unwrap() error { return e.err }

func CacheReadFailureStage(err error) string {
	var failure *cacheReadFailure
	if errors.As(err, &failure) {
		return failure.stage
	}
	return ""
}

func withCacheReadFailureStage(stage string, err error) error {
	if err == nil {
		return nil
	}
	var failure *cacheReadFailure
	if errors.As(err, &failure) {
		return err
	}
	return &cacheReadFailure{stage: stage, err: err}
}
