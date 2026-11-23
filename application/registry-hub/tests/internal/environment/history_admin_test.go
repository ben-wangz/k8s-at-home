package environment

import "testing"

func TestHistoryAdminConfigurationSeparatesCleanupPermission(t *testing.T) {
	configuration, _, err := newHistoryAdminConfiguration()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateHistoryAdminConfiguration(configuration); err != nil {
		t.Fatalf("valid history admin configuration rejected: %v", err)
	}
	if configuration.Basic[0].Password == configuration.Basic[1].Password ||
		configuration.Basic[1].Password == configuration.Basic[2].Password ||
		configuration.Basic[0].Password == configuration.Basic[2].Password {
		t.Fatal("history test principals share a password")
	}
	configuration.Basic[2].Permissions = []string{"task-admin", "history-cleanup"}
	if err := validateHistoryAdminConfiguration(configuration); err == nil {
		t.Fatal("task-admin credential with history-cleanup permission was accepted")
	}
}
