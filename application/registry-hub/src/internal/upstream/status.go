package upstream

import "errors"

func HTTPStatus(err error) (int, bool) {
	var statusError *HTTPStatusError
	if !errors.As(err, &statusError) {
		return 0, false
	}
	return statusError.StatusCode, true
}
