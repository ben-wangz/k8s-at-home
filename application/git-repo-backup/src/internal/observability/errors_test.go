package observability

import (
	"errors"
	"fmt"
	"testing"
)

func TestClassifyGitStderr(t *testing.T) {
	cases := map[string]string{
		"Host key verification failed.":                                  CodeHostKeyFailed,
		"git@host: Permission denied (publickey).":                       CodeAuthFailed,
		"fatal: Could not read from remote repository.":                  CodeRepoUnavailable,
		"fatal: repository not found":                                    CodeRepoUnavailable,
		"ssh: connect to host example.com port 22: Connection timed out": CodeRepoUnavailable,
		"error: object file is corrupt":                                  CodeGitFailed,
	}
	for stderr, want := range cases {
		if got := ClassifyGitStderr(stderr, false); got != want {
			t.Errorf("ClassifyGitStderr(%q) = %q, want %q", stderr, got, want)
		}
	}
	if got := ClassifyGitStderr("anything", true); got != CodeGitTimeout {
		t.Error("timeout dominates classification")
	}
}

func TestSafeErrorWrapping(t *testing.T) {
	inner := fmt.Errorf("secret-laden detail")
	err := WrapSafe(CodeGitFailed, "safe summary", inner)
	var se *SafeError
	if !errors.As(err, &se) {
		t.Fatal("must unwrap to SafeError")
	}
	if CodeOf(err) != CodeGitFailed {
		t.Fatal("code lost through wrapping")
	}
	if CodeOf(errors.New("plain")) != "internal_error" {
		t.Fatal("unknown errors get the fallback code")
	}
}
