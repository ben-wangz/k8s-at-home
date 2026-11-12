package app

import (
	"slices"
	"strings"
)

func (s *Service) configureWorkerBudgets(view *workerView) {
	view.ScheduleMode = "periodic"
	shared := slices.Contains(backgroundKinds, view.ID) || slices.Contains(backgroundScanKinds, view.ID) || strings.HasPrefix(view.ID, "statistics_") || view.ID == "activation_events"
	if shared {
		view.AdmissionPool, view.AdmissionCapacity = "ordinary", 2
		view.SharedConcurrency, view.AdmissionBudgetSeconds, view.CheckpointBudgetSeconds = 2, 15, 5
		view.ExecutionBudgetSeconds = 15
		if strings.HasPrefix(view.ID, "statistics_") {
			view.ExecutionBudgetSeconds = 10
		}
	}
	switch view.ID {
	case "cache_verification", "download_cleanup", "cache_cleanup", "distribution_verification":
		view.AdmissionPool, view.AdmissionCapacity = "solidification", cap(s.solidificationSlots)
		view.SharedConcurrency = view.AdmissionCapacity
		view.ExecutionBudgetSeconds = s.cacheVerificationBudget().Seconds()
	case "storage_inventory":
		view.AdmissionPool, view.AdmissionCapacity, view.SharedConcurrency = "storage_collector", 1, 0
		view.ExecutionBudgetSeconds, view.AdmissionBudgetSeconds, view.CheckpointBudgetSeconds = 15, 15, 5
	case "inventory_builder":
		view.ExecutionBudgetSeconds = s.inventoryConfig().BatchTimeout.Seconds()
		view.CheckpointBudgetSeconds = 0
	case "storage_probe", "registry_probe":
		view.ExecutionBudgetSeconds = 15
	case "diagnostic_events":
		view.ExecutionBudgetSeconds = 5
	case "index_repair":
		view.ExecutionBudgetSeconds, view.CheckpointBudgetSeconds = 30, protectionRecoveryCheckpointBudget.Seconds()
	case "protection_recovery":
		view.ExecutionBudgetSeconds, view.CheckpointBudgetSeconds = 60, protectionRecoveryCheckpointBudget.Seconds()
	}
}
