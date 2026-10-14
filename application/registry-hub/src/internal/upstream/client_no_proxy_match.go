package upstream

import (
	"net"
	"net/netip"
	"strings"
)

func noProxyMatch(host, port string, values ...string) bool {
	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			item = strings.TrimSpace(strings.ToLower(item))
			if item == "" {
				continue
			}
			if item == "*" {
				return true
			}
			if prefix, err := netip.ParsePrefix(item); err == nil {
				if address, parseErr := netip.ParseAddr(strings.Trim(host, "[]")); parseErr == nil && prefix.Contains(address) {
					return true
				}
				continue
			}
			entryHost := item
			if candidateHost, candidatePort, err := net.SplitHostPort(item); err == nil {
				if candidatePort != port {
					continue
				}
				entryHost = strings.ToLower(candidateHost)
			} else if strings.Contains(item, ":") && !strings.HasPrefix(item, "[") {
				continue
			}
			entryHost = strings.TrimPrefix(strings.Trim(entryHost, "[]"), ".")
			if strings.EqualFold(host, entryHost) || strings.HasSuffix(strings.ToLower(host), "."+entryHost) {
				return true
			}
		}
	}
	return false
}
