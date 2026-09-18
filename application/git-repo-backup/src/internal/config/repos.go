package config

import (
	"fmt"
	"os"
	"regexp"
)

// RepoNamePattern is the complete-match grammar for repository names.
var RepoNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// MaxRepoNameLen bounds repository name length.
const MaxRepoNameLen = 128

// ValidateRepoName enforces the archive-safe name grammar. Names are used to
// build filesystem and object paths, so the grammar is deliberately narrow
// and case-sensitive on case-sensitive filesystems.
func ValidateRepoName(name string) error {
	if name == "" {
		return fmt.Errorf("repository name must not be empty")
	}
	if len(name) > MaxRepoNameLen {
		return fmt.Errorf("repository name exceeds %d characters", MaxRepoNameLen)
	}
	if !RepoNamePattern.MatchString(name) {
		return fmt.Errorf("repository name contains unsupported characters")
	}
	return nil
}

// ValidateRepository validates one entry. Errors identify the repository by
// name or index only, never by URL content.
func ValidateRepository(idx int, repo Repository) error {
	if err := ValidateRepoName(repo.Name); err != nil {
		return fmt.Errorf("repository #%d: %w", idx, err)
	}
	if repo.URL == "" {
		return fmt.Errorf("repository %s: url must not be empty", repo.Name)
	}
	if _, err := ParseGitURL(repo.URL); err != nil {
		return fmt.Errorf("repository %s: url: %w", repo.Name, err)
	}
	return nil
}

// ValidateSSHRequirement rejects SSH repositories when the SSH material was
// explicitly disabled. HTTPS repositories remain usable without credentials;
// Git reports authentication_failed if a private HTTPS repository requires
// credentials that are not configured.
func ValidateSSHRequirement(repos []Repository, sshEnabled bool) error {
	if sshEnabled {
		return nil
	}
	for _, repo := range repos {
		parsed, err := ParseGitURL(repo.URL)
		if err != nil {
			return fmt.Errorf("repository %s: url: %w", repo.Name, err)
		}
		if parsed.Scheme == "ssh" {
			return fmt.Errorf("repository %s requires ssh credentials", repo.Name)
		}
	}
	return nil
}

// LoadRepositories reads and validates the repositories file. An empty list,
// duplicate names (case-sensitive), and any invalid entry are rejected.
func LoadRepositories(path string) ([]Repository, error) {
	data, err := readLimited(path, MaxRepositoriesSize)
	if err != nil {
		return nil, fmt.Errorf("read repositories file: %w", err)
	}
	var rf RepositoriesFile
	if err := strictDecode(path, data, &rf); err != nil {
		return nil, err
	}
	if len(rf.Repositories) == 0 {
		return nil, fmt.Errorf("repositories list must not be empty")
	}
	seen := make(map[string]struct{}, len(rf.Repositories))
	for i, repo := range rf.Repositories {
		if err := ValidateRepository(i, repo); err != nil {
			return nil, err
		}
		if _, dup := seen[repo.Name]; dup {
			return nil, fmt.Errorf("duplicate repository name %s", repo.Name)
		}
		seen[repo.Name] = struct{}{}
	}
	return rf.Repositories, nil
}

// FileExists reports whether path exists and is readable; used by validate
// to check local reference files without touching the network.
func FileExists(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("cannot access file: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("path is a directory, expected a file")
	}
	return nil
}
