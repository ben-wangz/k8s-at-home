package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/distribution"
	"registry-hub/internal/download"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/storage/s3store"
	"registry-hub/internal/tasks"
	"registry-hub/internal/upstream"
	"sync"
	"sync/atomic"
	"time"
)

type upstreamRequester interface {
	Do(context.Context, string, string, string, string, url.Values, http.Header) (*http.Response, error)
	Close()
}

type Service struct {
	cfg                  config.Config
	logger               *slog.Logger
	store                storage.Store
	registry             *distribution.Client
	upstream             upstreamRequester
	cache                *artifact.Cache
	downloader           *download.Manager
	downloadOnce         sync.Once
	tasks                *tasks.Manager
	records              *tasks.Records
	inventory            *inventory.Repository
	inventoryDirty       atomic.Bool
	inventoryInstalledAt time.Time
	gate                 *writeGate
	uploads              *uploadSessions
	metrics              *metrics
	cursorKey            []byte
	ready                atomic.Bool
	cacheProtectionReady atomic.Bool
	ctx                  context.Context
	cancel               context.CancelFunc
	clientSlots          chan struct{}
	noAuthSlots          chan struct{}
	pushSlots            chan struct{}
	upstreamSlots        chan struct{}
	solidificationSlots  chan struct{}
	blobIntentEvents     chan tasks.BlobIdentity
	cacheIntentRescan    atomic.Bool
	solidificationMu     sync.Mutex
	protectionRecoveryMu sync.Mutex
	newSolidifications   int
	referrerRepairs      int
	repairPending        bool
	upstreamPerKey       [256]chan struct{}
	limiter              *requestLimiter
	gc                   *gcController
	storageHealthy       atomic.Bool
	registryHealthy      atomic.Bool
	nativeMu             sync.Mutex
	nativeCache          map[string]nativeReferrerCapability
	closeOnce            sync.Once
	background           sync.WaitGroup
}

func New(cfg config.Config, logger *slog.Logger) (*Service, error) {
	serviceMetrics := newMetrics()
	var store storage.Store
	var err error
	switch cfg.StorageBackend {
	case "filesystem":
		store, err = filesystem.New(cfg.FilesystemRoot)
	case "s3":
		store, err = s3store.New(context.Background(), cfg.S3)
	default:
		err = errors.New("unsupported storage backend")
	}
	if err != nil {
		return nil, err
	}
	store = newObservedStore(store, serviceMetrics, logger)
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = storage.Probe(probeCtx, store)
	probeCancel()
	if err != nil {
		logger.Warn("storage conditional operation probe failed", "backend", cfg.StorageBackend, "error_class", "storage")
		_ = store.Close()
		return nil, err
	}
	cursorKey, err := loadCursorKey(context.Background(), store)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	registry, err := distribution.New(cfg)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	registry.SetObserver(func(event, result string) {
		if event == "api_error" {
			serviceMetrics.recordDistributionError(result)
		}
	})
	registryCtx, registryCancel := context.WithTimeout(context.Background(), 90*time.Second)
	err = pingWithRetry(registryCtx, registry.Ping, time.Second)
	registryCancel()
	if err != nil {
		registry.Close()
		_ = store.Close()
		return nil, err
	}
	remote, err := upstream.New(cfg)
	if err != nil {
		registry.Close()
		_ = store.Close()
		return nil, err
	}
	remote.SetObserver(serviceMetrics.recordEvent)
	gate, err := newWriteGate(context.Background(), store)
	if err != nil {
		remote.Close()
		registry.Close()
		_ = store.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{
		cfg: cfg, logger: logger, store: store, registry: registry, upstream: remote,
		cache: artifact.NewCache(store), gate: gate, metrics: serviceMetrics, cursorKey: cursorKey, ctx: ctx, cancel: cancel,
		records:     tasks.NewRecords(store),
		clientSlots: make(chan struct{}, 128), noAuthSlots: make(chan struct{}, 16),
		pushSlots: make(chan struct{}, 16), upstreamSlots: make(chan struct{}, 32),
		solidificationSlots: make(chan struct{}, cfg.Limits.Workers),
		blobIntentEvents:    make(chan tasks.BlobIdentity, 256),
		limiter:             newRequestLimiter(cfg.Limits.GlobalRate, cfg.Limits.GlobalBurst, cfg.Limits.NoAuthRate, cfg.Limits.NoAuthBurst), nativeCache: make(map[string]nativeReferrerCapability),
	}
	service.inventory = &inventory.Repository{Store: store}
	service.cache.EnableInventoryTracking()
	service.initializeInventory(ctx)
	if observed, ok := store.(*observedStore); ok {
		observed.inventoryNotify = func() { service.inventoryDirty.Store(true) }
	}
	for index := range service.upstreamPerKey {
		service.upstreamPerKey[index] = make(chan struct{}, 8)
	}
	service.records.SetMaxBlobIntents(cfg.Solidification.MaxBlobIntents)
	service.records.SetIdleTimeout(cfg.Solidification.IdleTimeout)
	service.storageHealthy.Store(true)
	service.registryHealthy.Store(true)
	recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	err = service.cache.RebuildRepresentationIndexes(recoveryCtx)
	recoveryCancel()
	if err != nil {
		cancel()
		remote.Close()
		registry.Close()
		_ = store.Close()
		return nil, err
	}
	service.gc, err = newGCController(service)
	if err != nil {
		cancel()
		remote.Close()
		registry.Close()
		_ = store.Close()
		return nil, err
	}
	processor := &solidifier{service: service, cfg: cfg, cache: service.cache, store: store, registry: registry, upstream: remote, gate: gate}
	service.tasks, err = tasks.NewManager(store, cfg.Limits.Workers, cfg.Limits.PendingTasks, processor)
	if err != nil {
		cancel()
		remote.Close()
		registry.Close()
		_ = store.Close()
		return nil, err
	}
	service.tasks.SetMaxWaitingRoots(cfg.Solidification.MaxWaitingRoots)
	service.tasks.SetIdleTimeout(cfg.Solidification.IdleTimeout)
	solidificationRecoveryCtx, solidificationRecoveryCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	if err := service.recoverSolidificationProtection(solidificationRecoveryCtx); err != nil {
		logger.Warn("solidification startup recovery is blocked", "error_class", "recovery", "error", err)
	}
	solidificationRecoveryCancel()
	if !gate.State().RecoveryBarrier {
		if err := service.wakeAvailableQuotaWaiter(context.Background(), "distribution_quota", gate.State()); err != nil {
			logger.Warn("startup quota waiter wake failed", "error_class", "storage")
		}
		if err := service.wakeAvailableQuotaWaiter(context.Background(), "protected_cache_capacity", gate.State()); err != nil {
			logger.Warn("startup protected cache quota waiter wake failed", "error_class", "storage")
		}
	}
	processor.tasks = service.tasks
	service.uploads = newUploadSessions(store, registry, service.tasks.Leases(), cfg.Limits.UploadIdleTimeout, cfg.Limits.UploadMaxLifetime)
	service.cache.SetOrphanProtection(service.protectCacheObject)
	service.tasks.Start(ctx)
	service.background.Add(6)
	go func() { defer service.background.Done(); service.runInventory(ctx) }()
	go func() {
		defer service.background.Done()
		service.watchReadiness(ctx)
	}()
	go func() {
		defer service.background.Done()
		service.refreshMetrics(ctx)
	}()
	go func() {
		defer service.background.Done()
		service.runReferrerRepairs(ctx)
	}()
	go func() {
		defer service.background.Done()
		service.runCacheCleanup(ctx)
	}()
	go func() {
		defer service.background.Done()
		service.runBlobIntentReconciliation(ctx)
	}()
	service.metrics.setGateState(gate.State().State)
	service.refreshReadiness()
	return service, nil
}
