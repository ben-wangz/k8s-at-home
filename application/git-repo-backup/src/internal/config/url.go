package config

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// SSHURL is the validated decomposition of a Git SSH URL. User, Host, and
// Port are only used for validation; Git always receives the original string
// as a single argument.
type SSHURL struct {
	User string
	Host string
	Port string
	Path string // repository path without leading slash
}

// ParseSSHURL accepts only ssh:// URLs and SCP-style Git URLs, including
// bracketed IPv6 hosts. Everything else — local paths, other schemes,
// passwords, query strings, fragments, control characters, whitespace, and
// shell metacharacters — is rejected. The single percent-decode performed by
// net/url for the ssh:// form is the only decoding applied.
func ParseSSHURL(raw string) (SSHURL, error) {
	if raw == "" {
		return SSHURL{}, fmt.Errorf("empty url")
	}
	if err := checkNoUnsafeRunes(raw); err != nil {
		return SSHURL{}, err
	}
	if strings.HasPrefix(raw, "ssh://") {
		return parseSSHScheme(raw)
	}
	if strings.Contains(raw, "://") {
		return SSHURL{}, fmt.Errorf("non-ssh scheme")
	}
	if strings.Contains(raw, ":") {
		return parseSCPStyle(raw)
	}
	return SSHURL{}, fmt.Errorf("not an ssh url")
}

// checkNoUnsafeRunes rejects control characters, whitespace, and backslashes
// anywhere in the input before any parsing happens.
func checkNoUnsafeRunes(raw string) error {
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("control character in url")
		}
		if unicode.IsSpace(r) {
			return fmt.Errorf("whitespace in url")
		}
		if r == '\\' {
			return fmt.Errorf("backslash in url")
		}
	}
	return nil
}

func parseSSHScheme(raw string) (SSHURL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return SSHURL{}, fmt.Errorf("unparseable ssh url")
	}
	if !strings.EqualFold(u.Scheme, "ssh") {
		return SSHURL{}, fmt.Errorf("non-ssh scheme")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.RawFragment != "" {
		return SSHURL{}, fmt.Errorf("query or fragment in url")
	}
	user := ""
	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			return SSHURL{}, fmt.Errorf("password in url")
		}
		user = u.User.Username()
		if user == "" {
			return SSHURL{}, fmt.Errorf("empty user in url")
		}
		if err := validateUser(user); err != nil {
			return SSHURL{}, err
		}
	}
	host := u.Hostname()
	if host == "" {
		return SSHURL{}, fmt.Errorf("empty host in url")
	}
	if err := validateHost(host); err != nil {
		return SSHURL{}, err
	}
	port := u.Port()
	if port != "" {
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return SSHURL{}, fmt.Errorf("invalid port in url")
		}
	}
	// u.Path is the net/url single percent-decode of the raw path.
	if u.Path == "" {
		return SSHURL{}, fmt.Errorf("empty path in url")
	}
	repoPath := strings.TrimPrefix(u.Path, "/")
	if repoPath == "" {
		return SSHURL{}, fmt.Errorf("empty repository path in url")
	}
	if err := validateRepoPath(repoPath); err != nil {
		return SSHURL{}, err
	}
	return SSHURL{User: user, Host: host, Port: port, Path: repoPath}, nil
}

func parseSCPStyle(raw string) (SSHURL, error) {
	spec := raw
	user := ""
	if idx := strings.LastIndex(spec, "@"); idx >= 0 {
		user = spec[:idx]
		spec = spec[idx+1:]
		if user == "" {
			return SSHURL{}, fmt.Errorf("empty user in url")
		}
		if strings.Contains(user, "@") {
			return SSHURL{}, fmt.Errorf("multiple @ in url user part")
		}
		if err := validateUser(user); err != nil {
			return SSHURL{}, err
		}
	}
	var host, repoPath string
	if strings.HasPrefix(spec, "[") {
		end := strings.Index(spec, "]")
		if end < 0 {
			return SSHURL{}, fmt.Errorf("unterminated ipv6 host in url")
		}
		host = spec[1:end]
		if net.ParseIP(host) == nil {
			return SSHURL{}, fmt.Errorf("invalid ipv6 host in url")
		}
		rest := spec[end+1:]
		if !strings.HasPrefix(rest, ":") {
			return SSHURL{}, fmt.Errorf("missing path separator after ipv6 host")
		}
		repoPath = rest[1:]
	} else {
		idx := strings.Index(spec, ":")
		if idx < 0 {
			return SSHURL{}, fmt.Errorf("missing path separator in url")
		}
		host = spec[:idx]
		repoPath = spec[idx+1:]
		if strings.Contains(repoPath, ":") {
			return SSHURL{}, fmt.Errorf("colon in scp-style path; use ssh:// form")
		}
	}
	if host == "" {
		return SSHURL{}, fmt.Errorf("empty host in url")
	}
	if err := validateHost(host); err != nil {
		return SSHURL{}, err
	}
	if repoPath == "" {
		return SSHURL{}, fmt.Errorf("empty repository path in url")
	}
	repoPath = strings.TrimPrefix(repoPath, "/")
	if repoPath == "" {
		return SSHURL{}, fmt.Errorf("empty repository path in url")
	}
	if err := validateRepoPath(repoPath); err != nil {
		return SSHURL{}, err
	}
	return SSHURL{User: user, Host: host, Path: repoPath}, nil
}

func validateUser(user string) error {
	if strings.HasPrefix(user, "-") {
		return fmt.Errorf("user must not start with '-'")
	}
	for _, r := range user {
		if isHostRune(r) {
			continue
		}
		return fmt.Errorf("unsupported character in url user")
	}
	return nil
}

// validateHost accepts bracket-stripped hostnames, IPv4, and IPv6 literals.
// Hosts starting with '-' are rejected so crafted values cannot be mistaken
// for command options.
func validateHost(host string) error {
	if strings.HasPrefix(host, "-") {
		return fmt.Errorf("host must not start with '-'")
	}
	if strings.Contains(host, ":") {
		if net.ParseIP(host) == nil {
			return fmt.Errorf("invalid ipv6 host in url")
		}
		return nil
	}
	if host == "" {
		return fmt.Errorf("empty host in url")
	}
	for _, r := range host {
		if isHostRune(r) {
			continue
		}
		return fmt.Errorf("unsupported character in url host")
	}
	return nil
}

func isHostRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '.' || r == '-' || r == '_':
		return true
	}
	return false
}

// validateRepoPath enforces the narrowed v1 repository-path grammar: ASCII
// letters, digits, '/', '.', '_', '-', '~', plus Unicode letters and digits.
// Shell metacharacters, spaces, and percent signs are rejected.
func validateRepoPath(path string) error {
	if strings.HasPrefix(path, "-") {
		return fmt.Errorf("repository path must not start with '-'")
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("repository path must not contain empty, '.', or '..' segments")
		}
	}
	for _, r := range path {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/' || r == '.' || r == '_' || r == '-' || r == '~':
		case r > 0x7f && (unicode.IsLetter(r) || unicode.IsDigit(r)):
		default:
			return fmt.Errorf("unsupported character in repository path")
		}
	}
	return nil
}
