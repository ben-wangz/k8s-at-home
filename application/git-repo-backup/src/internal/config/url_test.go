package config

import "testing"

func TestParseGitURLValid(t *testing.T) {
	cases := []struct {
		name   string
		url    string
		scheme string
		user   string
		host   string
		port   string
		path   string
	}{
		{"scp basic", "git@git.example.com:team/main.git", "ssh", "git", "git.example.com", "", "team/main.git"},
		{"scp no user", "git.example.com:team/main.git", "ssh", "", "git.example.com", "", "team/main.git"},
		{"scp root repo", "host:repo.git", "ssh", "", "host", "", "repo.git"},
		{"scp ipv6 bracket", "git@[2001:db8::1]:team/main.git", "ssh", "git", "2001:db8::1", "", "team/main.git"},
		{"ssh full", "ssh://git@git.example.com:2222/team/main.git", "ssh", "git", "git.example.com", "2222", "team/main.git"},
		{"ssh default port", "ssh://git@example.com/repo.git", "ssh", "git", "example.com", "", "repo.git"},
		{"ssh no user", "ssh://example.com/repo.git", "ssh", "", "example.com", "", "repo.git"},
		{"ssh ipv6", "ssh://git@[2001:db8::1]:22/repo.git", "ssh", "git", "2001:db8::1", "22", "repo.git"},
		{"ssh tilde path", "ssh://git@example.com/~user/repo.git", "ssh", "git", "example.com", "", "~user/repo.git"},
		{"ssh percent once", "ssh://git@example.com/a%2Db.git", "ssh", "git", "example.com", "", "a-b.git"},
		{"ssh unicode path", "ssh://git@example.com/team/répo.git", "ssh", "git", "example.com", "", "team/répo.git"},
		{"host underscore", "git@my_host:repo.git", "ssh", "git", "my_host", "", "repo.git"},
		{"https public", "https://github.com/ben-wangz/k8s-at-home.git", "https", "", "github.com", "", "ben-wangz/k8s-at-home.git"},
		{"https port", "https://git.example.com:8443/team/main.git", "https", "", "git.example.com", "8443", "team/main.git"},
		{"https uppercase scheme", "HTTPS://example.com/repo.git", "https", "", "example.com", "", "repo.git"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseGitURL(tc.url)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Scheme != tc.scheme || got.User != tc.user || got.Host != tc.host || got.Port != tc.port || got.Path != tc.path {
				t.Fatalf("got %+v want scheme=%q user=%q host=%q port=%q path=%q", got, tc.scheme, tc.user, tc.host, tc.port, tc.path)
			}
		})
	}
}

func TestParseGitURLInvalid(t *testing.T) {
	cases := []string{
		"",
		" ",
		"git@example.com:repo\n.git",
		"git@example.com:repo .git",
		"/local/path.git",
		"../relative.git",
		"http://example.com/repo.git",
		"git+ssh://example.com/repo.git",
		"https://example.com",
		"https:///repo.git",
		"https://example.com/",
		"https://user@example.com/repo.git",
		"https://user:pass@example.com/repo.git",
		"https://example.com/repo.git?token=1",
		"https://example.com/repo.git#frag",
		"https://example.com/repo.git?",
		"https://example.com/repo%20.git",
		"https://example.com/a/../b.git",
		"https://example.com:0/repo.git",
		"https://example.com:99999/repo.git",
		"ssh://user:pass@example.com/repo.git",
		"ssh://example.com/repo.git?token=1",
		"ssh://example.com/repo.git#frag",
		"ssh:///repo.git",
		"ssh://example.com/",
		"ssh://example.com",
		"ssh://@example.com/repo.git",
		"ssh://-evil.com/repo.git",
		"git@-evil:repo.git",
		"git@example.com:-repo.git",
		"ssh://example.com/-repo.git",
		"ssh://example.com/a\\b.git",
		"ssh://git@example.com/a%20b.git",
		"git@example.com:sh;ll.git",
		"git@example.com:pa th.git",
		"git@example.com:$(cmd).git",
		"git@example.com:repo`x`.git",
		"git@example.com:repo|pipe.git",
		"ssh://example.com/repo%252520x.git",
		"git@example.com:a:b:c.git",
		"git@[2001:db8::1/repo.git",
		"git@example.com:",
		"ssh://example.com:0/repo.git",
		"ssh://example.com:99999/repo.git",
		"ssh://example.com/a/../b.git",
		"ssh://example.com/./repo.git",
		"git@user@evil.com:repo.git",
		"git@example.com:repo..git/../x",
	}
	for _, url := range cases {
		t.Run(url, func(t *testing.T) {
			if _, err := ParseGitURL(url); err == nil {
				t.Fatalf("expected rejection for %q", url)
			}
		})
	}
}
