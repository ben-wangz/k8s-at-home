package testkit

import "testing"

func TestS3MultipartPartCount(t *testing.T) {
	for _, test := range []struct {
		etag  string
		count int
		valid bool
	}{
		{etag: `"b484e3c42043e7b385465d4481b34aa2-2"`, count: 2, valid: true},
		{etag: `"single-part-etag"`},
		{etag: `"multipart-etag-invalid"`},
	} {
		count, valid := S3MultipartPartCount(test.etag)
		if count != test.count || valid != test.valid {
			t.Errorf("S3MultipartPartCount(%q) = %d, %v; want %d, %v", test.etag, count, valid, test.count, test.valid)
		}
	}
}
