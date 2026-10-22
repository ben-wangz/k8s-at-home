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
)

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
