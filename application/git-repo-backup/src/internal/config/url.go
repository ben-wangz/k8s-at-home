package config

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// GitURL is the validated decomposition of a Git URL. User, Host, and Port
// are only used for validation; Git always receives the original string as a
// single argument.
type GitURL struct {
	Scheme string
	User   string
	Host   string
	Port   string
	Path   string // repository path without leading slash
}

// SSHURL is retained for callers that used the original decomposition type.
// ParseGitURL is the canonical parser and accepts SSH and HTTPS URLs.
type SSHURL struct {
	User string
	Host string
	Port string
	Path string
}

// ParseGitURL accepts ssh:// URLs, SCP-style SSH URLs, and https:// URLs,
// including bracketed IPv6 hosts. Local paths, other schemes, URL credentials,
// query strings, fragments, control characters, whitespace, and shell
// metacharacters are rejected. The single percent-decode performed by
// net/url for scheme URLs is the only decoding applied.
func ParseGitURL(raw string) (GitURL, error) {
	if raw == "" {
		return GitURL{}, fmt.Errorf("empty url")
	}
	if err := checkNoUnsafeRunes(raw); err != nil {
		return GitURL{}, err
	}
	if schemeEnd := strings.Index(raw, "://"); schemeEnd >= 0 {
		switch strings.ToLower(raw[:schemeEnd]) {
		case "ssh":
			return parseSSHScheme(raw)
		case "https":
			return parseHTTPScheme(raw)
		default:
			return GitURL{}, fmt.Errorf("unsupported url scheme")
		}
	}
	if strings.Contains(raw, ":") {
		return parseSCPStyle(raw)
	}
	return GitURL{}, fmt.Errorf("not a git url")
}

// ParseSSHURL is a compatibility wrapper. It now accepts the complete set of
// supported Git URLs while preserving the original return type.
func ParseSSHURL(raw string) (SSHURL, error) {
	parsed, err := ParseGitURL(raw)
	if err != nil {
		return SSHURL{}, err
	}
	return SSHURL{User: parsed.User, Host: parsed.Host, Port: parsed.Port, Path: parsed.Path}, nil
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

func parseSSHScheme(raw string) (GitURL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return GitURL{}, fmt.Errorf("unparseable ssh url")
	}
	if !strings.EqualFold(u.Scheme, "ssh") {
		return GitURL{}, fmt.Errorf("non-ssh scheme")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return GitURL{}, fmt.Errorf("query or fragment in url")
	}
	user := ""
	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			return GitURL{}, fmt.Errorf("password in url")
		}
		user = u.User.Username()
		if user == "" {
			return GitURL{}, fmt.Errorf("empty user in url")
		}
		if err := validateUser(user); err != nil {
			return GitURL{}, err
		}
	}
	host := u.Hostname()
	if host == "" {
		return GitURL{}, fmt.Errorf("empty host in url")
	}
	if err := validateHost(host); err != nil {
		return GitURL{}, err
	}
	port := u.Port()
	if port != "" {
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return GitURL{}, fmt.Errorf("invalid port in url")
		}
	}
	// u.Path is the net/url single percent-decode of the raw path.
	if u.Path == "" {
		return GitURL{}, fmt.Errorf("empty path in url")
	}
	repoPath := strings.TrimPrefix(u.Path, "/")
	if repoPath == "" {
		return GitURL{}, fmt.Errorf("empty repository path in url")
	}
	if err := validateRepoPath(repoPath); err != nil {
		return GitURL{}, err
	}
	return GitURL{Scheme: "ssh", User: user, Host: host, Port: port, Path: repoPath}, nil
}

func parseHTTPScheme(raw string) (GitURL, error) {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "https") {
		return GitURL{}, fmt.Errorf("unparseable https url")
	}
	if u.User != nil {
		return GitURL{}, fmt.Errorf("credentials in https url")
	}
	if u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return GitURL{}, fmt.Errorf("query or fragment in url")
	}
	host := u.Hostname()
	if host == "" {
		return GitURL{}, fmt.Errorf("empty host in url")
	}
	if err := validateHost(host); err != nil {
		return GitURL{}, err
	}
	port := u.Port()
	if port != "" {
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return GitURL{}, fmt.Errorf("invalid port in url")
		}
	}
	if u.Path == "" {
		return GitURL{}, fmt.Errorf("empty path in url")
	}
	repoPath := strings.TrimPrefix(u.Path, "/")
	if repoPath == "" {
		return GitURL{}, fmt.Errorf("empty repository path in url")
	}
	if err := validateRepoPath(repoPath); err != nil {
		return GitURL{}, err
	}
	return GitURL{Scheme: "https", Host: host, Port: port, Path: repoPath}, nil
}

func parseSCPStyle(raw string) (GitURL, error) {
	spec := raw
	user := ""
	if idx := strings.LastIndex(spec, "@"); idx >= 0 {
		user = spec[:idx]
		spec = spec[idx+1:]
		if user == "" {
			return GitURL{}, fmt.Errorf("empty user in url")
		}
		if strings.Contains(user, "@") {
			return GitURL{}, fmt.Errorf("multiple @ in url user part")
		}
		if err := validateUser(user); err != nil {
			return GitURL{}, err
		}
	}
	var host, repoPath string
	if strings.HasPrefix(spec, "[") {
		end := strings.Index(spec, "]")
		if end < 0 {
			return GitURL{}, fmt.Errorf("unterminated ipv6 host in url")
		}
		host = spec[1:end]
		if net.ParseIP(host) == nil {
			return GitURL{}, fmt.Errorf("invalid ipv6 host in url")
		}
		rest := spec[end+1:]
		if !strings.HasPrefix(rest, ":") {
			return GitURL{}, fmt.Errorf("missing path separator after ipv6 host")
		}
		repoPath = rest[1:]
	} else {
		idx := strings.Index(spec, ":")
		if idx < 0 {
			return GitURL{}, fmt.Errorf("missing path separator in url")
		}
		host = spec[:idx]
		repoPath = spec[idx+1:]
		if strings.Contains(repoPath, ":") {
			return GitURL{}, fmt.Errorf("colon in scp-style path; use ssh:// form")
		}
	}
	if host == "" {
		return GitURL{}, fmt.Errorf("empty host in url")
	}
	if err := validateHost(host); err != nil {
		return GitURL{}, err
	}
	if repoPath == "" {
		return GitURL{}, fmt.Errorf("empty repository path in url")
	}
	repoPath = strings.TrimPrefix(repoPath, "/")
	if repoPath == "" {
		return GitURL{}, fmt.Errorf("empty repository path in url")
	}
	if err := validateRepoPath(repoPath); err != nil {
		return GitURL{}, err
	}
	return GitURL{Scheme: "ssh", User: user, Host: host, Path: repoPath}, nil
}
