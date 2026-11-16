package imageinventory

import (
	"context"
	"fmt"
	"math"
	"net/url"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func verifyFreshnessAgreement(ctx context.Context, environment testkit.CaseContext, query url.Values) (map[string]any, error) {
	for range 3 {
		page, err := read(ctx, environment, "/api/v1/images?"+query.Encode())
		if err != nil {
			return nil, err
		}
		status, err := read(ctx, environment, "/api/v1/images/status")
		if err != nil {
			return nil, err
		}
		snapshot := testkit.Map(testkit.Map(status["background"])["snapshot"])
		if testkit.String(snapshot["snapshotID"]) != testkit.String(page["snapshotID"]) || testkit.String(snapshot["lastSourceCheckAt"]) != testkit.String(page["lastSourceCheckAt"]) {
			continue
		}
		direct, background := testkit.Map(page["freshness"]), testkit.Map(snapshot["freshness"])
		directAge, directOK := direct["ageSeconds"].(float64)
		backgroundAge, backgroundOK := background["ageSeconds"].(float64)
		if !directOK || !backgroundOK || testkit.String(direct["state"]) == "" || testkit.String(direct["state"]) != testkit.String(background["state"]) || math.Abs(directAge-backgroundAge) > 5 {
			return nil, fmt.Errorf("inventory/background freshness differs: inventory=%v background=%v", direct, background)
		}
		return map[string]any{"snapshotID": snapshot["snapshotID"], "lastSourceCheckAt": snapshot["lastSourceCheckAt"], "inventory": direct, "background": background}, nil
	}
	return nil, fmt.Errorf("inventory snapshot changed during three freshness comparisons")
}
