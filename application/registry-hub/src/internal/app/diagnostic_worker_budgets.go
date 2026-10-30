package app

import (
	"slices"
	"strings"
)

func (s *Service) configureWorkerBudgets(view *workerView) {
	view.ScheduleMode = "periodic"
	shared := slices.Contains(backgroundKinds, view.ID) || slices.Contains(backgroundScanKinds, view.ID) || strings.HasPrefix(view.ID, "statistics_") || view.ID == "activation_events"
	if shared {
		view.SharedConcurrency, view.AdmissionBudgetSeconds, view.CheckpointBudgetSeconds = 2, 15, 5
		view.ExecutionBudgetSeconds = 15
		if strings.HasPrefix(view.ID, "statistics_") {
			view.ExecutionBudgetSeconds = 10
		}
	}
	switch view.ID {
	case "inventory_builder":
		view.ExecutionBudgetSeconds = s.inventoryConfig().BatchTimeout.Seconds()
		view.CheckpointBudgetSeconds = 0
	case "storage_probe", "registry_probe":
		view.ExecutionBudgetSeconds = 15
	case "diagnostic_events":
		view.ExecutionBudgetSeconds = 5
	case "index_repair":
		view.ExecutionBudgetSeconds = 300
	}
}
