package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"registry-hub/internal/config"
)

const (
	Pull          = "pull"
	Catalog       = "catalog"
	Push          = "push"
	Delete        = "delete"
	TaskAdmin     = "task-admin"
	ReferrerPrune = "referrer-prune"
	GCAdmin       = "gc-admin"
	Observe       = "observe"
	WorkRetry     = "work-retry"
)

type Principal struct {
	Name        string
	Permissions map[string]struct{}
	Trusted     bool
}

func Authenticate(request *http.Request, configured config.Auth, allowAnonymous bool) (Principal, bool) {
	if configured.Empty() {
		if allowAnonymous {
			return Principal{Name: "anonymous", Trusted: true}, true
		}
		return Principal{}, false
	}
	value := request.Header.Get("Authorization")
	scheme, credential, found := strings.Cut(value, " ")
	if !found {
		return Principal{}, false
	}
	switch strings.ToLower(scheme) {
	case "basic":
		username, password, ok := request.BasicAuth()
		if !ok {
			return Principal{}, false
		}
		for _, item := range configured.Basic {
			if secureEqual(username, item.Username) && secureEqual(password, item.Password) {
				return principal(item.Username, item.Permissions), true
			}
		}
	case "bearer":
		for index, item := range configured.Bearer {
			if secureEqual(credential, item.Token) {
				name := item.Username
				if name == "" {
					name = "bearer-" + string(rune('1'+index))
				}
				return principal(name, item.Permissions), true
			}
		}
	}
	return Principal{}, false
}

func Challenge(writer http.ResponseWriter, realm string) {
	writer.Header().Set("WWW-Authenticate", `Basic realm="`+realm+`", Bearer realm="`+realm+`"`)
}

func (p Principal) Allows(permission string) bool {
	if p.Trusted {
		return true
	}
	_, allowed := p.Permissions[permission]
	if permission == Observe || permission == WorkRetry {
		_, legacy := p.Permissions[TaskAdmin]
		allowed = allowed || legacy
	}
	return allowed
}

func principal(name string, permissions []string) Principal {
	set := make(map[string]struct{}, len(permissions))
	for _, permission := range permissions {
		set[permission] = struct{}{}
	}
	return Principal{Name: name, Permissions: set}
}

func secureEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
