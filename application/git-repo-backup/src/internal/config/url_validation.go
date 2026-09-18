package config

import (
	"fmt"
	"net"
	"strings"
	"unicode"
)

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
