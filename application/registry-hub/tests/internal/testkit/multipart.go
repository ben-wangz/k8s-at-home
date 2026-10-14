package testkit

import (
	"strconv"
	"strings"
)

func S3MultipartPartCount(etag string) (int, bool) {
	etag = strings.Trim(strings.TrimSpace(etag), `"`)
	separator := strings.LastIndexByte(etag, '-')
	if separator < 0 {
		return 0, false
	}
	count, err := strconv.Atoi(etag[separator+1:])
	return count, err == nil && count > 1
}
