package app

import "github.com/prometheus/client_golang/prometheus"

func newMetrics() *metrics {
	result := &metrics{
		backgroundDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "registry_hub_background_unit_duration_seconds", Help: "Background work unit duration.", Buckets: prometheus.DefBuckets}, []string{"kind", "result"}),
		backgroundWait:     prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "registry_hub_background_queue_wait_seconds", Help: "Time from durable observation to first background processing.", Buckets: []float64{1, 5, 15, 60, 300, 1800, 3600}}, []string{"kind"}),
		registry:           prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_hub_requests_total", Help: "Client Registry requests.",
		}, []string{"operation", "method", "status", "cache_source", "error_class"}),
		requestLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "registry_hub_request_duration_seconds", Help: "Client Registry request latency.",
			Buckets: prometheus.DefBuckets,
		}, []string{"operation", "method", "status", "cache_source", "error_class"}),
		upstream: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_hub_upstream_requests_total", Help: "Public Registry request outcomes.",
		}, []string{"result"}),
		tasks: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_hub_tasks", Help: "Persisted solidification tasks by state.",
		}, []string{"state"}),
		taskKinds: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_hub_tasks_by_kind_state", Help: "Persisted solidification tasks by bounded kind and state.",
		}, []string{"kind", "state"}),
		taskAuthorities: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_hub_tasks_by_authority_class", Help: "Persisted solidification tasks by state and authority class.",
		}, []string{"state", "authority_class"}),
		taskErrors: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_hub_task_errors", Help: "Persisted solidification tasks by stable error class.",
		}, []string{"class"}),
		taskWaitReasons: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_hub_task_waiting", Help: "Waiting solidification tasks by bounded wait reason.",
		}, []string{"reason"}),
		taskRetries: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_hub_task_retry_attempts", Help: "Persisted task retry attempts by bounded kind and state.",
		}, []string{"kind", "state"}),
		taskCancelPhases: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_hub_task_cancellations", Help: "Tasks with cancellation intent by bounded phase.",
		}, []string{"phase"}),
		blobIntents: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_hub_blob_intents", Help: "Persisted blob intents by bounded state.",
		}, []string{"state"}),
		blobIntentLimit: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_hub_blob_intent_limit", Help: "Configured maximum count of active blob intents.",
		}),
		taskRunnable: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_hub_tasks_runnable", Help: "Tasks eligible for a solidification worker.",
		}),
		taskWaiting: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_hub_tasks_waiting", Help: "Tasks waiting for cache, dependencies, or capacity.",
		}),
		recoveryUnknown: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_hub_solidification_recovery_unknown", Help: "Tasks retained with unresolved protection recovery state.",
		}),
		gcBlockers: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_hub_gc_blockers", Help: "Solidification sessions blocking GC by bounded reason.",
		}, []string{"reason"}),
		gate: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_hub_gc_gate", Help: "Whether the GC write gate is in a named state.",
		}, []string{"state"}),
		concurrency: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_hub_concurrency_slots", Help: "Currently occupied process-local concurrency slots.",
		}, []string{"gate"}),
		rejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_hub_rejections_total", Help: "Requests rejected by a rate or concurrency gate.",
		}, []string{"gate"}),
		events: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_hub_events_total", Help: "Low-cardinality Registry Hub events.",
		}, []string{"event"}),
		gcRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_hub_gc_runs_total", Help: "Distribution GC results.",
		}, []string{"result"}),
		gcDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "registry_hub_gc_duration_seconds", Help: "Distribution GC execution duration.",
			Buckets: prometheus.DefBuckets,
		}),
		storageOps: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_hub_storage_operations_total", Help: "Storage operations by result.",
		}, []string{"operation", "result"}),
		storageLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "registry_hub_storage_operation_duration_seconds", Help: "Storage operation latency.",
			Buckets: prometheus.DefBuckets,
		}, []string{"operation", "result"}),
		storageWarnings: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_hub_storage_warnings_total", Help: "Unexpected storage failures that were logged as warnings.",
		}, []string{"operation", "result"}),
		objectLeases: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_hub_object_leases", Help: "Object download leases by local state.",
		}, []string{"state"}),
		cacheObjects: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_hub_cache_objects", Help: "Verified cached content objects.",
		}),
		cacheBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_hub_cache_bytes", Help: "Bytes held by verified cached content objects.",
		}),
		cacheStaging: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_hub_cache_staging_objects", Help: "Recent cache objects without a complete commit record.",
		}),
		cacheOrphans: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_hub_cache_orphan_objects", Help: "Stale cache objects without a complete commit record.",
		}),
		cacheCleanup: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_hub_cache_cleanup_pending_objects", Help: "Verified cache objects pending cleanup.",
		}),
		distributionErr: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_hub_distribution_api_errors_total", Help: "Internal Distribution API errors by bounded result.",
		}, []string{"result"}),
		distributionVal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_hub_distribution_response_validation_failures_total", Help: "Invalid internal Distribution responses by bounded validation class.",
		}, []string{"class"}),
		referrerSources: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_hub_referrer_reads_total", Help: "Referrer reads by authoritative source.",
		}, []string{"source"}),
		referrerRepair: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_hub_referrer_repair_pending", Help: "Whether fallback referrer repair work is pending.",
		}),
		referrerReconcile: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_hub_referrer_reconcile_total", Help: "Referrer reconciliation outcomes.",
		}, []string{"result"}),
		protectionSessions: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_hub_solidification_protection_sessions", Help: "Persistent Distribution protection sessions by bounded state.",
		}, []string{"state"}),
		protectionReserved: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_hub_solidification_distribution_reserved_bytes", Help: "Distribution reservation bytes by reservation class.",
		}, []string{"class"}),
		protectionLimit: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_hub_solidification_distribution_reservation_limit_bytes", Help: "Configured limit for new unanchored Distribution reservations.",
		}),
		protectedCacheReserved: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_hub_solidification_protected_cache_reserved_bytes", Help: "Cache bytes reserved as inputs for active blob solidification.",
		}),
		protectedCacheLimit: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_hub_solidification_protected_cache_limit_bytes", Help: "Configured limit for active blob solidification cache inputs.",
		}),
		recoveryBarrier: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_hub_solidification_recovery_barrier", Help: "Whether solidification recovery blocks new Registry writes.",
		}),
		partialReserved: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_hub_download_partial_reserved_bytes", Help: "Reserved bytes for active resumable upstream downloads.",
		}),
		partialSessions: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_hub_download_partial_sessions", Help: "Active resumable upstream download sessions.",
		}),
		partialLimit: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_hub_download_partial_limit_bytes", Help: "Configured aggregate resumable download reservation limit.",
		}),
		s3ReplayMemory: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_hub_s3_replay_memory_bytes", Help: "Backend S3 request replay memory in use.",
		}),
		s3ReplayDisk: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_hub_s3_replay_disk_bytes", Help: "Backend S3 request replay temporary disk in use.",
		}),
		s3ReplayActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_hub_s3_replay_active_sources", Help: "Active Backend S3 request replay sources.",
		}),
	}
	result.registry.MustRegister(result.requests, result.requestLatency, result.upstream, result.tasks, result.taskKinds, result.taskAuthorities, result.taskErrors, result.taskWaitReasons, result.taskRetries, result.taskCancelPhases, result.blobIntents, result.blobIntentLimit, result.taskRunnable, result.taskWaiting, result.recoveryUnknown, result.gcBlockers, result.gate, result.concurrency, result.rejections, result.events, result.gcRuns, result.gcDuration, result.storageOps, result.storageLatency, result.storageWarnings, result.objectLeases, result.cacheObjects, result.cacheBytes, result.cacheStaging, result.cacheOrphans, result.cacheCleanup, result.distributionErr, result.distributionVal, result.referrerSources, result.referrerRepair, result.referrerReconcile, result.protectionSessions, result.protectionReserved, result.protectionLimit, result.protectedCacheReserved, result.protectedCacheLimit, result.recoveryBarrier, result.partialReserved, result.partialSessions, result.partialLimit, result.s3ReplayMemory, result.s3ReplayDisk, result.s3ReplayActive, result.backgroundDuration, result.backgroundWait)
	result.objectLeases.WithLabelValues("active").Set(0)
	result.objectLeases.WithLabelValues("waiting").Set(0)
	result.referrerRepair.Set(0)
	for _, state := range []string{"protected", "anchoring", "stop_requested", "quiescing", "quiesced", "anchored", "recovery_required", "other"} {
		result.protectionSessions.WithLabelValues(state).Set(0)
	}
	for _, class := range []string{"new", "legacy"} {
		result.protectionReserved.WithLabelValues(class).Set(0)
	}
	result.protectionLimit.Set(0)
	result.protectedCacheReserved.Set(0)
	result.protectedCacheLimit.Set(0)
	result.recoveryBarrier.Set(0)
	result.taskRunnable.Set(0)
	result.taskWaiting.Set(0)
	result.recoveryUnknown.Set(0)
	result.partialReserved.Set(0)
	result.partialSessions.Set(0)
	result.partialLimit.Set(0)
	result.s3ReplayMemory.Set(0)
	result.s3ReplayDisk.Set(0)
	result.s3ReplayActive.Set(0)
	for _, reason := range taskWaitReasonLabels {
		result.taskWaitReasons.WithLabelValues(reason).Set(0)
	}
	for _, phase := range []string{"stop_requested", "quiesced", "other"} {
		result.taskCancelPhases.WithLabelValues(phase).Set(0)
	}
	for _, state := range blobIntentMetricStates {
		result.blobIntents.WithLabelValues(state).Set(0)
	}
	result.blobIntentLimit.Set(0)
	for _, reason := range []string{"executing", "unanchored", "cancellation", "unknown", "anchored", "other"} {
		result.gcBlockers.WithLabelValues(reason).Set(0)
	}
	for _, class := range []string{"headers", "manifest", "list", "referrers", "upload", "other"} {
		result.distributionVal.WithLabelValues(class).Add(0)
	}
	for _, event := range []string{"upstream_retry", "upstream_404", "upstream_fetch", "upstream_auth_challenge", "upstream_redirect", "local_hit", "digest_mismatch", "lease_lost", "download_wait_timeout", "download_resume", "download_persisted_chunk", "download_fallback", "download_recovered_corrupt_chunk", "download_partial_cleanup"} {
		result.events.WithLabelValues(event).Add(0)
	}
	result.initInventoryMetrics()
	result.initBackgroundBudgetMetrics()
	return result
}
