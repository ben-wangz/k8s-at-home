package testkit

import (
	"context"
	"testing"
)

func TestValidateCasesChecksRunAfterReferences(t *testing.T) {
	tests := []struct {
		name     string
		runAfter []string
	}{
		{name: "unknown", runAfter: []string{"missing"}},
		{name: "self", runAfter: []string{"case"}},
		{name: "duplicate", runAfter: []string{"other", "other"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cases := []Case{
				{Definition: Definition{ID: "case", RunAfter: test.runAfter}, Run: func(_ context.Context, _ CaseContext) error { return nil }},
				{Definition: Definition{ID: "other"}, Run: func(_ context.Context, _ CaseContext) error { return nil }},
			}
			if _, err := ValidateCases(cases); err == nil {
				t.Fatal("expected invalid runAfter reference to fail validation")
			}
		})
	}
}
