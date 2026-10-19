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
	cfg                 config.Config
	logger              *slog.Logger
	store               storage.Store
	registry            *distribution.Client
	upstream            upstreamRequester
	cache               *artifact.Cache
	downloader          *download.Manager
	downloadOnce        sync.Once
	tasks               *tasks.Manager
	gate                *writeGate
	uploads             *uploadSessions
	metrics             *metrics
	cursorKey           []byte
	ready               atomic.Bool
	ctx                 context.Context
	cancel              context.CancelFunc
	clientSlots         chan struct{}
	noAuthSlots         chan struct{}
	pushSlots           chan struct{}
	upstreamSlots       chan struct{}
	solidificationSlots chan struct{}
	solidificationMu    sync.Mutex
	newSolidifications  int
	referrerRepairs     int
	repairPending       bool
	upstreamPerKey      [256]chan struct{}
	limiter             *requestLimiter
	gc                  *gcController
	nativeMu            sync.Mutex
	nativeCache         map[string]nativeReferrerCapability
	closeOnce           sync.Once
	background          sync.WaitGroup
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
		clientSlots: make(chan struct{}, 128), noAuthSlots: make(chan struct{}, 16),
		pushSlots: make(chan struct{}, 16), upstreamSlots: make(chan struct{}, 32),
		solidificationSlots: make(chan struct{}, cfg.Limits.Workers),
		limiter:             newRequestLimiter(cfg.Limits.GlobalRate, cfg.Limits.GlobalBurst, cfg.Limits.NoAuthRate, cfg.Limits.NoAuthBurst), nativeCache: make(map[string]nativeReferrerCapability),
	}
	for index := range service.upstreamPerKey {
		service.upstreamPerKey[index] = make(chan struct{}, 8)
	}
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
	processor.tasks = service.tasks
	service.uploads = newUploadSessions(store, registry, service.tasks.Leases(), cfg.Limits.UploadIdleTimeout, cfg.Limits.UploadMaxLifetime)
	service.cache.SetOrphanProtection(service.protectCacheObject)
	service.tasks.Start(ctx)
	service.background.Add(3)
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
	service.metrics.setGateState(gate.State().State)
	service.ready.Store(true)
	return service, nil
}

func pingWithRetry(ctx context.Context, ping func(context.Context) error, interval time.Duration) error {
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return errors.Join(errors.New("Distribution did not become ready before startup deadline"), lastErr, err)
			}
			return err
		}
		pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		lastErr = ping(pingCtx)
		cancel()
		if lastErr == nil {
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(errors.New("Distribution did not become ready before startup deadline"), lastErr, ctx.Err())
		case <-timer.C:
		}
	}
}

func (s *Service) ClientHandler() http.Handler {
	return http.HandlerFunc(s.handleClient)
}

func (s *Service) downloadManager() *download.Manager {
	s.downloadOnce.Do(func() {
		s.downloader = download.New(s.store, s.cfg.Limits.DownloadMaxBytes)
	})
	return s.downloader
}

func (s *Service) SetReady(ready bool) { s.ready.Store(ready) }

// BeginShutdown stops background admission while the HTTP servers drain active requests.
func (s *Service) BeginShutdown() {
	s.ready.Store(false)
	s.cancel()
}
