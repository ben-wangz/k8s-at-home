package config

import (
	"strings"
	"testing"
)

func TestValidateRepoName(t *testing.T) {
	valid := []string{"a", "A1", "repo.git", "my-repo_1", strings.Repeat("x", 128)}
	for _, name := range valid {
		if err := ValidateRepoName(name); err != nil {
			t.Errorf("expected %q valid: %v", name, err)
		}
	}
	invalid := []string{
		"",
		"-repo", ".repo", "_repo",
		"re po", "re/po", "re:po", "re\\po",
		strings.Repeat("x", 129),
	}
	for _, name := range invalid {
		if err := ValidateRepoName(name); err == nil {
			t.Errorf("expected %q invalid", name)
		}
	}
}

func TestLoadRepositories(t *testing.T) {
	body := `
repositories:
  - name: example-main
    url: git@git.example.com:team/main.git
  - name: other
    url: ssh://git@git.example.com:2222/team/other.git
  - name: public
    url: https://github.com/ben-wangz/k8s-at-home.git
`
	repos, err := LoadRepositories(writeTemp(t, "repositories.yaml", body))
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 3 || repos[0].Name != "example-main" {
		t.Fatalf("unexpected repos: %+v", repos)
	}
}

func TestLoadRepositoriesRejects(t *testing.T) {
	cases := map[string]string{
		"empty list":        "repositories: []\n",
		"missing list":      "other: 1\n",
		"duplicate name":    "repositories:\n  - {name: a, url: git@h:x.git}\n  - {name: a, url: git@h:y.git}\n",
		"bad name":          "repositories:\n  - {name: ../evil, url: git@h:x.git}\n",
		"empty url":         "repositories:\n  - {name: a, url: ''}\n",
		"url with password": "repositories:\n  - {name: a, url: 'ssh://u:p@h/x.git'}\n",
		"unknown field":     "repositories:\n  - {name: a, url: git@h:x.git, extra: 1}\n",
		"extra document":    "repositories:\n  - {name: a, url: git@h:x.git}\n---\nrepositories: []\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadRepositories(writeTemp(t, "repositories.yaml", body)); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestValidateSSHRequirement(t *testing.T) {
	repos := []Repository{
		{Name: "public", URL: "https://github.com/ben-wangz/k8s-at-home.git"},
		{Name: "private", URL: "git@github.com:owner/private.git"},
	}
	if err := ValidateSSHRequirement(repos[:1], false); err != nil {
		t.Fatalf("public HTTPS repository must not require SSH: %v", err)
	}
	if err := ValidateSSHRequirement(repos, false); err == nil {
		t.Fatal("SSH repository must be rejected when SSH is disabled")
	}
	if err := ValidateSSHRequirement(repos, true); err != nil {
		t.Fatalf("SSH-enabled configuration must accept mixed repositories: %v", err)
	}
}
