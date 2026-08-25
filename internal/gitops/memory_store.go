package gitops

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// MemoryStore is an in-memory Store, for tests and for running the server
// without a remote. Every Update that changes something bumps the revision.
type MemoryStore struct {
	mu       sync.Mutex
	files    map[string][]byte
	revision int
	// Messages records the commit messages, oldest first.
	Messages []string
}

// NewMemoryStore returns a store holding the given files.
func NewMemoryStore(files map[string]string) *MemoryStore {
	s := &MemoryStore{files: map[string][]byte{}}
	for p, content := range files {
		cleaned, err := cleanPath(p)
		if err != nil {
			panic(err)
		}
		s.files[cleaned] = []byte(content)
	}
	return s
}

// Files returns a copy of the current content, for assertions.
func (s *MemoryStore) Files() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.files))
	for p, data := range s.files {
		out[p] = string(data)
	}
	return out
}

func (s *MemoryStore) View(_ context.Context, fn func(r Reader) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(&memoryTx{files: s.files})
}

func (s *MemoryStore) Update(_ context.Context, message string, fn func(tx Tx) error) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	working := make(map[string][]byte, len(s.files))
	for p, data := range s.files {
		working[p] = data
	}
	tx := &memoryTx{files: working}
	if err := fn(tx); err != nil {
		return "", err
	}
	if !tx.changed(s.files) {
		return s.rev(), nil
	}
	s.files = working
	s.revision++
	s.Messages = append(s.Messages, message)
	return s.rev(), nil
}

func (s *MemoryStore) rev() string { return fmt.Sprintf("mem-%d", s.revision) }

type memoryTx struct {
	files map[string][]byte
}

func (t *memoryTx) changed(before map[string][]byte) bool {
	if len(before) != len(t.files) {
		return true
	}
	for p, data := range t.files {
		if old, ok := before[p]; !ok || string(old) != string(data) {
			return true
		}
	}
	return false
}

func (t *memoryTx) ReadFile(p string) ([]byte, error) {
	cleaned, err := cleanPath(p)
	if err != nil {
		return nil, err
	}
	data, ok := t.files[cleaned]
	if !ok {
		return nil, fmt.Errorf("%s: %w", cleaned, ErrNotFound)
	}
	return append([]byte(nil), data...), nil
}

func (t *memoryTx) ReadDir(p string) ([]string, error) {
	cleaned, err := cleanPath(p)
	if err != nil {
		return nil, err
	}
	prefix := cleaned + "/"
	if cleaned == "." {
		prefix = ""
	}
	seen := map[string]bool{}
	for f := range t.files {
		if !strings.HasPrefix(f, prefix) {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(f, prefix), "/")
		seen[name] = true
	}
	if len(seen) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (t *memoryTx) Exists(p string) bool {
	cleaned, err := cleanPath(p)
	if err != nil {
		return false
	}
	if _, ok := t.files[cleaned]; ok {
		return true
	}
	names, _ := t.ReadDir(cleaned)
	return len(names) > 0
}

func (t *memoryTx) WriteFile(p string, data []byte) error {
	cleaned, err := cleanPath(p)
	if err != nil {
		return err
	}
	t.files[cleaned] = append([]byte(nil), data...)
	return nil
}

func (t *memoryTx) Remove(p string) error {
	cleaned, err := cleanPath(p)
	if err != nil {
		return err
	}
	delete(t.files, cleaned)
	prefix := cleaned + "/"
	for f := range t.files {
		if strings.HasPrefix(f, prefix) {
			delete(t.files, f)
		}
	}
	return nil
}
