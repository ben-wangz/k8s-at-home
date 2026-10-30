package config

import (
	"errors"
	"fmt"
	"strings"
)

func validateAuth(auth Auth) error {
	const allowed = "pull catalog push delete task-admin referrer-prune gc-admin observe work-retry"
	for _, credential := range append(append([]Credential{}, auth.Basic...), auth.Bearer...) {
		if len(credential.Permissions) == 0 {
			return errors.New("each credential must declare at least one permission")
		}
		for _, permission := range credential.Permissions {
			if !strings.Contains(" "+allowed+" ", " "+permission+" ") {
				return fmt.Errorf("unknown permission %q", permission)
			}
		}
	}
	for _, credential := range auth.Basic {
		if credential.Username == "" || credential.Password == "" {
			return errors.New("Basic credentials require username and password")
		}
	}
	for _, credential := range auth.Bearer {
		if credential.Token == "" {
			return errors.New("Bearer credentials require token")
		}
	}
	return nil
}
