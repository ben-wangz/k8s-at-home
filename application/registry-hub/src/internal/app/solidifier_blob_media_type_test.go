package app

import "testing"

func TestBlobCacheMediaType(t *testing.T) {
	tests := []struct {
		name       string
		descriptor string
		response   string
		want       string
	}{
		{
			name:       "descriptor media type is authoritative",
			descriptor: "application/vnd.oci.image.config.v1+json",
			response:   "application/octet-stream",
			want:       "application/vnd.oci.image.config.v1+json",
		},
		{
			name:     "response media type fills a missing descriptor",
			response: "application/json; charset=utf-8",
			want:     "application/json",
		},
		{
			name:     "invalid response media type falls back to binary",
			response: "not a media type",
			want:     "application/octet-stream",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := blobCacheMediaType(test.descriptor, test.response); got != test.want {
				t.Fatalf("blobCacheMediaType() = %q, want %q", got, test.want)
			}
		})
	}
}
