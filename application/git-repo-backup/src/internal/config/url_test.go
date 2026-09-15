package config

import "testing"

func TestParseSSHURLValid(t *testing.T) {
	cases := []struct {
		name string
		url  string
		user string
		host string
		port string
		path string
	}{
		{"scp basic", "git@git.example.com:team/main.git", "git", "git.example.com", "", "team/main.git"},
		{"scp no user", "git.example.com:team/main.git", "", "git.example.com", "", "team/main.git"},
		{"scp root repo", "host:repo.git", "", "host", "", "repo.git"},
		{"scp ipv6 bracket", "git@[2001:db8::1]:team/main.git", "git", "2001:db8::1", "", "team/main.git"},
		{"ssh full", "ssh://git@git.example.com:2222/team/main.git", "git", "git.example.com", "2222", "team/main.git"},
		{"ssh default port", "ssh://git@example.com/repo.git", "git", "example.com", "", "repo.git"},
		{"ssh no user", "ssh://example.com/repo.git", "", "example.com", "", "repo.git"},
		{"ssh ipv6", "ssh://git@[2001:db8::1]:22/repo.git", "git", "2001:db8::1", "22", "repo.git"},
		{"ssh tilde path", "ssh://git@example.com/~user/repo.git", "git", "example.com", "", "~user/repo.git"},
		{"ssh percent once", "ssh://git@example.com/a%2Db.git", "git", "example.com", "", "a-b.git"},
		{"ssh unicode path", "ssh://git@example.com/team/répo.git", "git", "example.com", "", "team/répo.git"},
		{"host underscore", "git@my_host:repo.git", "git", "my_host", "", "repo.git"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSSHURL(tc.url)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.User != tc.user || got.Host != tc.host || got.Port != tc.port || got.Path != tc.path {
				t.Fatalf("got %+v want user=%q host=%q port=%q path=%q", got, tc.user, tc.host, tc.port, tc.path)
			}
		})
	}
}

func TestParseSSHURLInvalid(t *testing.T) {
	cases := []string{
		"",
		" ",
		"git@example.com:repo\n.git",
		"git@example.com:repo .git",
		"/local/path.git",
		"../relative.git",
		"https://example.com/repo.git",
		"http://example.com/repo.git",
		"git+ssh://example.com/repo.git",
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
			if _, err := ParseSSHURL(url); err == nil {
				t.Fatalf("expected rejection for %q", url)
			}
		})
	}
}
