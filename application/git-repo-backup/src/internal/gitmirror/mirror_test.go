package gitmirror

import (
	"strings"
	"testing"
)

func TestParseLsRemoteHead(t *testing.T) {
	cases := []struct {
		name     string
		out      string
		hasHead  bool
		symbolic bool
		unborn   bool
		ref      string
		oid      string
	}{
		{
			name:     "symbolic head",
			out:      "ref: refs/heads/main\tHEAD\n6789abc\tHEAD\n",
			hasHead:  true,
			symbolic: true,
			ref:      "refs/heads/main",
			oid:      "6789abc",
		},
		{
			name:    "detached head",
			out:     "6789abc\tHEAD\n",
			hasHead: true,
			oid:     "6789abc",
		},
		{
			name:     "unborn head",
			out:      "ref: refs/heads/main\tHEAD\n0000000000000000000000000000000000000000\tHEAD\n",
			hasHead:  true,
			symbolic: true,
			unborn:   true,
			ref:      "refs/heads/main",
			oid:      "0000000000000000000000000000000000000000",
		},
		{name: "empty repository", out: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseLsRemoteHead(tc.out)
			if err != nil {
				t.Fatal(err)
			}
			if got.HasHead != tc.hasHead || got.Symbolic != tc.symbolic ||
				got.Unborn != tc.unborn || got.Ref != tc.ref || got.OID != tc.oid {
				t.Fatalf("got %+v", got)
			}
		})
	}
	if _, err := ParseLsRemoteHead("ref: refs/heads/x\tOTHER\n"); err == nil {
		t.Fatal("non-HEAD symref line must be rejected")
	}
}

func TestRenderBareConfig(t *testing.T) {
	out := string(RenderBareConfig(ConfigInfo{
		RepositoryFormatVersion: "1", ObjectFormat: "sha256", RemoteURL: PlaceholderRemoteURL,
	}))
	for _, want := range []string{
		"repositoryformatversion = 1", "bare = true",
		"objectformat = sha256",
		"url = " + PlaceholderRemoteURL,
		"fetch = +refs/*:refs/*", `mirror = true`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Count(out, "[") != 3 {
		t.Fatalf("unexpected sections in:\n%s", out)
	}
}
