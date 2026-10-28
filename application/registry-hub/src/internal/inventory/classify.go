package inventory

func Classify(image Image, anchored bool) string {
	if image.Evidence.Freshness != "fresh" {
		return "unknown"
	}
	if image.Evidence.ClosureComplete && anchored && image.Availability.State == "complete" && (image.Scope != "reference" || image.Reference == image.ResolvedDigest || image.TagPublished) {
		return "solidified"
	}
	active, failed, cancelled := false, false, false
	outcome := false
	for _, work := range image.Solidification.Tasks {
		switch work.Status {
		case "pending", "running", "retry_wait", "waiting_cache", "waiting_dependencies", "waiting_capacity", "verified_local", "repair_pending":
			active = true
		case "failed":
			if !outcome {
				failed = true
				outcome = true
			}
		case "cancelled":
			if !outcome {
				cancelled = true
				outcome = true
			}
		case "solidified":
			outcome = true
		}
	}
	started := image.Solidification.Started
	if image.Cache.State == "complete" && started != nil && !*started && !failed && !cancelled {
		return "cached_not_started"
	}
	if active {
		return "solidifying"
	}
	if failed {
		return "failed"
	}
	if cancelled {
		return "cancelled"
	}
	if image.Cache.State == "complete" && started == nil {
		return "cached_start_unknown"
	}
	if !image.Evidence.ClosureComplete || image.Cache.State == "unknown" || image.Availability.State == "unknown" {
		return "unknown"
	}
	if image.Availability.State != "complete" {
		return "waiting_cache"
	}
	return "not_scheduled"
}

func Execution(works []Work) (string, *Work) {
	ranks := map[string]int{"running": 5, "pending": 4, "retry_wait": 3, "waiting_cache": 2, "waiting_dependencies": 2, "waiting_capacity": 2, "verified_local": 2, "repair_pending": 2}
	var selected *Work
	for index := range works {
		if works[index].HistoryUnavailable {
			continue
		}
		if ranks[works[index].Status] > 0 && (selected == nil || ranks[works[index].Status] > ranks[selected.Status]) {
			selected = &works[index]
		}
	}
	if selected == nil {
		if len(works) == 0 {
			return "not_scheduled", nil
		}
		return "idle", nil
	}
	switch selected.Status {
	case "running":
		return "running", selected
	case "pending":
		return "queued", selected
	case "retry_wait":
		return "retry_wait", selected
	default:
		return "waiting", selected
	}
}
