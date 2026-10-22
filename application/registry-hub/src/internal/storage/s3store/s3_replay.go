package s3store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const (
	defaultReplayMemoryBytes    int64 = 16 << 20
	defaultReplayMaxMemoryBytes int64 = 64 << 20
	defaultReplayMaxDiskBytes   int64 = 1 << 40
	defaultReplayDirectory            = "/tmp/registry-hub-s3-replay"
	replayFilePrefix                  = ".registry-hub-replay-"
)

type replayConfig struct {
	perSourceMemory int64
	maxMemory       int64
	maxDisk         int64
	directory       string
}

type replayManager struct {
	config    replayConfig
	lockFile  *os.File
	removeDir bool

	mu         sync.Mutex
	changed    chan struct{}
	memoryUsed int64
	diskUsed   int64
	active     int
	closing    bool
	closed     bool
}

func (m *replayManager) usage() (int64, int64, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.memoryUsed, m.diskUsed, m.active
}

type replaySource struct {
	manager  *replayManager
	size     int64
	digest   [sha256.Size]byte
	data     []byte
	path     string
	charged  int64
	inMemory bool
	once     sync.Once
}

type replayRange struct {
	io.ReadSeeker
	close func() error
	once  sync.Once
	err   error
}

func newReplayManager(config replayConfig) (*replayManager, error) {
	config = config.withDefaults()
	if config.perSourceMemory <= 0 || config.maxMemory <= 0 || config.maxDisk <= 0 || config.perSourceMemory > config.maxMemory || !filepath.IsAbs(config.directory) {
		return nil, errors.New("S3 replay limits and directory are invalid")
	}
	if err := os.MkdirAll(config.directory, 0700); err != nil {
		return nil, fmt.Errorf("create S3 replay directory: %w", err)
	}
	lockFile, err := os.OpenFile(filepath.Join(config.directory, ".registry-hub-replay.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open S3 replay directory lock: %w", err)
	}
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("S3 replay directory is already in use: %w", err)
	}
	manager := &replayManager{config: config, lockFile: lockFile, changed: make(chan struct{})}
	if err := manager.removeStaleFiles(); err != nil {
		_ = manager.releaseLock()
		return nil, err
	}
	return manager, nil
}

func (c replayConfig) withDefaults() replayConfig {
	if c.perSourceMemory == 0 {
		c.perSourceMemory = defaultReplayMemoryBytes
	}
	if c.maxMemory == 0 {
		c.maxMemory = defaultReplayMaxMemoryBytes
	}
	if c.maxDisk == 0 {
		c.maxDisk = defaultReplayMaxDiskBytes
	}
	if c.directory == "" {
		c.directory = defaultReplayDirectory
	}
	return c
}

func (m *replayManager) prepare(ctx context.Context, source io.Reader, size int64) (*replaySource, error) {
	if source == nil || size < 0 {
		return nil, errors.New("S3 replay source and size are required")
	}
	inMemory := size <= m.config.perSourceMemory && size <= int64(maxInt()) && m.reserveMemory(size)
	if !inMemory && !m.reserveDisk(size) {
		return nil, errors.New("S3 replay resource budget is exhausted")
	}
	replay := &replaySource{manager: m, size: size, charged: size, inMemory: inMemory}
	if !inMemory {
		file, err := os.CreateTemp(m.config.directory, replayFilePrefix+"*.tmp")
		if err != nil {
			m.release(size, false, true)
			return nil, fmt.Errorf("create S3 replay file: %w", err)
		}
		replay.path = file.Name()
		if err := replay.consume(ctx, source, file); err != nil {
			_ = file.Close()
			replay.Close()
			return nil, err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			replay.Close()
			return nil, fmt.Errorf("sync S3 replay file: %w", err)
		}
		if err := file.Close(); err != nil {
			replay.Close()
			return nil, fmt.Errorf("close S3 replay file: %w", err)
		}
		return replay, nil
	}
	replay.data = make([]byte, 0, int(size))
	if err := replay.consume(ctx, source, nil); err != nil {
		replay.Close()
		return nil, err
	}
	return replay, nil
}
