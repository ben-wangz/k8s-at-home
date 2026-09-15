package storage

import "testing"

func TestBackupIDRoundTrip(t *testing.T) {
	id := "20260914T020000Z"
	ts, err := ParseBackupID(id)
	if err != nil {
		t.Fatal(err)
	}
	if FormatBackupID(ts) != id {
		t.Fatalf("round trip failed: %s", FormatBackupID(ts))
	}
}

func TestParseBackupIDRejects(t *testing.T) {
	for _, id := range []string{
		"", "20260914T020000", "20260914t020000z", "20261314T020000Z",
		"20260914T990000Z", "not-a-backup", "20260914T020000Z/", "20260914T020000Z-1",
	} {
		if _, err := ParseBackupID(id); err == nil {
			t.Errorf("expected %q rejected", id)
		}
	}
}
