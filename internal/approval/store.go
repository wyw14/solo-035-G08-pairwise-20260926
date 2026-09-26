package approval

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Store durably appends approval timeline entries as JSON lines.
type Store struct {
	mu      sync.Mutex
	logPath string
}

func NewStore(directory string) (*Store, error) {
	if directory == "" {
		return nil, fmt.Errorf("approval directory is required")
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, fmt.Errorf("create approval directory: %w", err)
	}
	return &Store{logPath: filepath.Join(directory, "approvals.jsonl")}, nil
}

func (store *Store) Append(entry LogEntry) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	file, err := os.OpenFile(store.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open approval log: %w", err)
	}
	if err := json.NewEncoder(file).Encode(entry); err != nil {
		file.Close()
		return fmt.Errorf("append approval log: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync approval log: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close approval log: %w", err)
	}
	return nil
}

func (store *Store) ReadAll() ([]LogEntry, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	file, err := os.Open(store.logPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open approval log: %w", err)
	}
	defer file.Close()
	var entries []LogEntry
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var entry LogEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return nil, fmt.Errorf("truncated approval entry: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan approval log: %w", err)
	}
	return entries, nil
}

func (store *Store) Path() string {
	return store.logPath
}
