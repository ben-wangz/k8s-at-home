package main

import (
	"fmt"
	"strconv"
	"strings"
)

func (m targetMatcher) Match(target string) bool {
	if _, ok := m.exact[target]; ok {
		return true
	}

	for _, suffix := range m.suffix {
		if len(target) > len(suffix) && strings.HasSuffix(target, suffix) {
			return true
		}
	}

	return false
}

func parseAllowedTargets(raw string) targetMatcher {
	matcher := targetMatcher{exact: map[string]struct{}{}, suffix: []string{}}
	for _, token := range strings.Split(raw, ",") {
		t := strings.TrimSpace(token)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "*.") && len(t) > 2 {
			matcher.suffix = append(matcher.suffix, t[1:])
			continue
		}
		matcher.exact[t] = struct{}{}
	}
	return matcher
}

func parseRoute(raw string) (route, error) {
	parts := strings.Split(raw, "+")
	if len(parts) != 2 && len(parts) != 3 {
		return route{}, fmt.Errorf("invalid username format")
	}

	r := route{
		upstreamUser: parts[0],
		target:       parts[1],
		port:         22,
	}

	if r.upstreamUser == "" || r.target == "" {
		return route{}, fmt.Errorf("invalid username format")
	}

	if len(parts) == 3 {
		port, err := strconv.Atoi(parts[2])
		if err != nil || port < 1 || port > 65535 {
			return route{}, fmt.Errorf("invalid port")
		}
		r.port = port
	}

	return r, nil
}
