package s3store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
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

func (r *replaySource) consume(ctx context.Context, source io.Reader, file *os.File) error {
	hash := sha256.New()
	buffer := make([]byte, 256<<10)
	remaining := r.size
	noProgress := 0
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		length := int64(len(buffer))
		if remaining < length {
			length = remaining
		}
		read, readErr := source.Read(buffer[:int(length)])
		if read > 0 {
			noProgress = 0
			chunk := buffer[:read]
			if file != nil {
				written, writeErr := file.Write(chunk)
				if writeErr != nil {
					return fmt.Errorf("write S3 replay file: %w", writeErr)
				}
				if written != len(chunk) {
					return io.ErrShortWrite
				}
			} else {
				r.data = append(r.data, chunk...)
			}
			_, _ = hash.Write(chunk)
			remaining -= int64(read)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) && remaining == 0 {
				break
			}
			if errors.Is(readErr, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			return fmt.Errorf("read S3 replay source: %w", readErr)
		}
		if read == 0 {
			noProgress++
			if noProgress >= 100 {
				return io.ErrNoProgress
			}
		}
	}
	var extra [1]byte
	for noProgress = 0; ; noProgress++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		read, err := source.Read(extra[:])
		if read > 0 {
			return errors.New("S3 replay source exceeds the declared size")
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("verify S3 replay source length: %w", err)
		}
		if noProgress >= 99 {
			return io.ErrNoProgress
		}
	}
	copy(r.digest[:], hash.Sum(nil))
	return nil
}

func (r *replaySource) OpenRange(offset, length int64) (*replayRange, error) {
	if offset < 0 || length < 0 || offset > r.size || length > r.size-offset {
		return nil, errors.New("S3 replay range is outside the prepared source")
	}
	if r.inMemory {
		section := io.NewSectionReader(bytes.NewReader(r.data), offset, length)
		return &replayRange{ReadSeeker: section, close: func() error { return nil }}, nil
	}
	file, err := os.Open(r.path)
	if err != nil {
		return nil, fmt.Errorf("open S3 replay file: %w", err)
	}
	section := io.NewSectionReader(file, offset, length)
	return &replayRange{ReadSeeker: section, close: file.Close}, nil
}

func (r *replayRange) Close() error {
	r.once.Do(func() { r.err = r.close() })
	return r.err
}

func (r *replaySource) Close() {
	r.once.Do(func() {
		canReleaseDisk := true
		if r.path != "" {
			if err := os.Remove(r.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				canReleaseDisk = false
			}
		}
		r.data = nil
		r.manager.release(r.charged, r.inMemory, canReleaseDisk)
	})
}

func (m *replayManager) reserveMemory(size int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing || m.closed || size > m.config.maxMemory-m.memoryUsed {
		return false
	}
	m.memoryUsed += size
	m.active++
	return true
}

func (m *replayManager) reserveDisk(size int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing || m.closed || size > m.config.maxDisk-m.diskUsed {
		return false
	}
	m.diskUsed += size
	m.active++
	return true
}

func (m *replayManager) release(size int64, inMemory, canReleaseDisk bool) {
	m.mu.Lock()
	if inMemory {
		m.memoryUsed -= size
	} else if canReleaseDisk {
		m.diskUsed -= size
	}
	m.active--
	close(m.changed)
	m.changed = make(chan struct{})
	m.mu.Unlock()
}

func (m *replayManager) Close() error {
	m.mu.Lock()
	m.closing = true
	for m.active > 0 {
		changed := m.changed
		m.mu.Unlock()
		<-changed
		m.mu.Lock()
	}
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.mu.Unlock()
	removeErr := m.removeStaleFiles()
	lockErr := m.releaseLock()
	var directoryErr error
	if m.removeDir {
		lockPath := filepath.Join(m.config.directory, ".registry-hub-replay.lock")
		if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			directoryErr = err
		}
		if err := os.Remove(m.config.directory); err != nil && !errors.Is(err, os.ErrNotExist) {
			directoryErr = errors.Join(directoryErr, err)
		}
	}
	return errors.Join(removeErr, lockErr, directoryErr)
}

func (m *replayManager) removeStaleFiles() error {
	entries, err := os.ReadDir(m.config.directory)
	if err != nil {
		return fmt.Errorf("list S3 replay directory: %w", err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), replayFilePrefix) || !entry.Type().IsRegular() {
			continue
		}
		if err := os.Remove(filepath.Join(m.config.directory, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale S3 replay file: %w", err)
		}
	}
	return nil
}

func (m *replayManager) releaseLock() error {
	if m.lockFile == nil {
		return nil
	}
	unlockErr := unix.Flock(int(m.lockFile.Fd()), unix.LOCK_UN)
	closeErr := m.lockFile.Close()
	m.lockFile = nil
	return errors.Join(unlockErr, closeErr)
}

func maxInt() int { return int(^uint(0) >> 1) }
