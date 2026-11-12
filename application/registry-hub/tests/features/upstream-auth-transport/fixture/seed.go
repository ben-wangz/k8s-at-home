package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func seedTaskRecords(source, destination string) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return fmt.Errorf("read task seeds: %w", err)
	}
	if err := os.MkdirAll(destination, 0750); err != nil {
		return fmt.Errorf("create task state directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			return fmt.Errorf("read task seed %s: %w", entry.Name(), err)
		}
		if err := publishTaskSeed(destination, entry.Name(), content); err != nil {
			return fmt.Errorf("publish task seed %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func publishTaskSeed(directory, name string, content []byte) error {
	temporary, err := os.CreateTemp(directory, ".quota-seed-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(content); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if err := temporary.Sync(); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	// Linking a complete file preserves records already written by another initializer.
	if err := os.Link(temporary.Name(), filepath.Join(directory, name)); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return nil
}
