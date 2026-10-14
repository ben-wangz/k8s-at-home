package route

import (
	"encoding/hex"
	"net"
	"regexp"
	"strconv"
	"strings"
)

func ValidateAuthority(authority string) error {
	if authority == "" || strings.ContainsAny(authority, "/@?#\\\r\n\x00") {
		return ErrInvalidPath
	}
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		if strings.HasPrefix(authority, "[") && strings.HasSuffix(authority, "]") {
			host = strings.Trim(authority, "[]")
		} else if strings.Count(authority, ":") == 0 {
			host = authority
		} else {
			return ErrInvalidPath
		}
	} else if port == "" {
		return ErrInvalidPath
	}
	if host == "" || strings.ContainsAny(host, " %") {
		return ErrInvalidPath
	}
	if net.ParseIP(host) == nil {
		if len(host) > 253 || strings.HasSuffix(host, ".") {
			return ErrInvalidPath
		}
		for _, label := range strings.Split(host, ".") {
			if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return ErrInvalidPath
			}
			for _, r := range label {
				if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
					return ErrInvalidPath
				}
			}
		}
	}
	if port != "" {
		n, parseErr := strconv.Atoi(port)
		if parseErr != nil || n < 1 || n > 65535 {
			return ErrInvalidPath
		}
	}
	return nil
}

func ValidReference(reference string) bool {
	if strings.HasPrefix(reference, "sha256:") || strings.Contains(reference, ":") {
		return ValidDigest(reference)
	}
	if len(reference) == 0 || len(reference) > 128 || !tagStart.MatchString(reference[:1]) {
		return false
	}
	for _, r := range reference[1:] {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_.-", r)) {
			return false
		}
	}
	return true
}

func ValidDigest(value string) bool {
	algorithm, encoded, found := strings.Cut(value, ":")
	if !found || algorithm == "" || len(encoded) < 32 || len(encoded)%2 != 0 {
		return false
	}
	if !ValidDigestAlgorithm(algorithm) {
		return false
	}
	_, err := hex.DecodeString(encoded)
	return err == nil
}

func ValidDigestAlgorithm(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '+' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func ValidateRepository(repository string) error {
	if repository == "" || len(repository) > 1024 {
		return ErrInvalidPath
	}
	parts := strings.Split(repository, "/")
	if len(parts) == 0 {
		return ErrInvalidPath
	}
	if len(parts) > 1 {
		if err := ValidateAuthority(parts[0]); err != nil {
			return err
		}
		parts = parts[1:]
	}
	for _, part := range parts {
		if part == "" || !ordinary.MatchString(part) {
			return ErrInvalidPath
		}
	}
	return nil
}

var ordinary = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*$`)
var tagStart = regexp.MustCompile(`^[A-Za-z0-9_]$`)
