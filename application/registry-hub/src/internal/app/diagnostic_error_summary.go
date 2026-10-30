package app

func stableDiagnosticError(class string) string {
	switch class {
	case "timeout", "storage", "storage_unavailable", "cancelled", "corrupt_source", "source_missing", "capacity", "distribution", "object_busy", "protection_state", "task_state", "generation":
		return class
	default:
		return "operation_failed; classified source details unavailable"
	}
}
