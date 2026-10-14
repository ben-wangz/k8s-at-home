package app

import (
	"bytes"
	"io"
	"testing"
)

func TestParseContentRange(t *testing.T) {
	start, end, total, err := parseContentRange("bytes 10-19/100")
	if err != nil {
		t.Fatal(err)
	}
	if start != 10 || end != 19 || total != 100 {
		t.Fatalf("range = %d-%d/%d", start, end, total)
	}
	if _, _, _, err := parseContentRange("bytes 10-19/*"); err == nil {
		t.Fatal("wildcard total should be rejected")
	}
}

func TestSpoolResponseBoundsAndDigestWriter(t *testing.T) {
	digest := bytes.NewBuffer(nil)
	spool, size, err := spoolResponse(bytes.NewReader([]byte("abc")), 3, digest)
	if err != nil {
		t.Fatal(err)
	}
	defer removeSpool(spool)
	if size != 3 {
		t.Fatalf("size = %d, want 3", size)
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(spool)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "abc" || digest.String() != "abc" {
		t.Fatalf("spooled data = %q, digest data = %q", data, digest.String())
	}
	if _, _, err := spoolResponse(bytes.NewReader([]byte("abcd")), 3, nil); err == nil {
		t.Fatal("oversized response should be rejected")
	}
}
