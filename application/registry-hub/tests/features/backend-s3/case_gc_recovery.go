package backends3

import (
	"context"
	"fmt"
	"net/http"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func verifyBackendS3GCRecovery(ctx context.Context, environment testkit.CaseContext) (map[string]any, error) {
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/gc", nil, nil)
	if err != nil {
		return nil, err
	}
	var status map[string]any
	if _, err := support.DecodeJSON(response, 2<<20, &status); err != nil {
		return nil, err
	}
	gate := testkit.Map(status["gate"])
	if testkit.String(gate["state"]) != "open" {
		return nil, fmt.Errorf("GC gate did not complete startup recovery after backend restart")
	}
	if recoveryBarrier, exists := gate["recoveryBarrier"]; exists && recoveryBarrier != false {
		return nil, fmt.Errorf("GC gate retained its startup recovery barrier after backend restart")
	}
	if recoveryReason, exists := gate["recoveryReason"]; exists && testkit.String(recoveryReason) != "" {
		return nil, fmt.Errorf("GC gate did not complete startup recovery after backend restart")
	}
	return map[string]any{
		"state":           testkit.String(gate["state"]),
		"recoveryBarrier": gate["recoveryBarrier"],
		"epoch":           gate["epoch"],
	}, nil
}
