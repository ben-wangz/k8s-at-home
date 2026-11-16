package diagnostics

import (
	"fmt"
	"slices"
)

func verifySummaryCoverage(snapshot report) error {
	state := snapshot.Domains["state"]
	if !slices.Equal(state.SummaryOnlyCategories, []string{"storage_inventory_output"}) {
		return fmt.Errorf("storage report does not declare internal output summary coverage")
	}
	if snapshot.Cost.InputObjects < 1 || snapshot.Cost.OutputObjects < 1 || snapshot.Cost.OutputBytes < 1 {
		return fmt.Errorf("storage report lacks collection cost evidence")
	}
	if state.Categories["storage_inventory_output"].Objects != snapshot.Cost.SummaryOnlyObjects {
		return fmt.Errorf("internal output cost differs from category accounting")
	}
	return nil
}
