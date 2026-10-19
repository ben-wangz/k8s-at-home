package download

import "fmt"

type StatusError struct {
	Code       int
	RetryAfter string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("upstream blob returned status %d", e.Code)
}
