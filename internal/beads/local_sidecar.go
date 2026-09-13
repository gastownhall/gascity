package beads

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/gastownhall/gascity/internal/fsys"
)

// localSidecar persists clone-local key-value data (see Store.SetLocalString)
// to a JSON file on disk, keyed by bead ID then key. It backs both BdStore
// and NativeDoltStore, the two Store implementations whose durable state
// lives in an external process/DB rather than an in-process slice, so
// neither can simply keep an unexported map next to its beads the way
// MemStore does. The sidecar never participates in Dolt sync or the bd
// subprocess path; it is read and written independently of both.
//
// A zero-value path means "in-memory only, never persisted" — used by
// test-only constructors that have no workdir to anchor a file under.
type localSidecar struct {
	path string

	mu     sync.Mutex
	data   map[string]map[string]string
	loaded bool
}

// newLocalSidecar returns a sidecar backed by the JSON file at path. The
// file is read lazily on first use, not at construction. An empty path
// makes the sidecar in-memory-only.
func newLocalSidecar(path string) *localSidecar {
	return &localSidecar{path: path}
}

func (l *localSidecar) ensureLoadedLocked() error {
	if l.loaded {
		return nil
	}
	l.loaded = true
	if l.path == "" {
		l.data = make(map[string]map[string]string)
		return nil
	}
	raw, err := os.ReadFile(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			l.data = make(map[string]map[string]string)
			return nil
		}
		return fmt.Errorf("loading local sidecar %q: %w", l.path, err)
	}
	data := make(map[string]map[string]string)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &data); err != nil {
			return fmt.Errorf("parsing local sidecar %q: %w", l.path, err)
		}
	}
	l.data = data
	return nil
}

// Set stores value under (id, key), or clears it when value is "". Mirrors
// Store.SetLocalString and, like that method, never validates that id
// refers to an existing bead.
func (l *localSidecar) Set(id, key, value string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.ensureLoadedLocked(); err != nil {
		return err
	}
	if value == "" {
		if l.data[id] == nil {
			return nil
		}
		delete(l.data[id], key)
		if len(l.data[id]) == 0 {
			delete(l.data, id)
		}
	} else {
		if l.data[id] == nil {
			l.data[id] = make(map[string]string)
		}
		l.data[id][key] = value
	}
	return l.saveLocked()
}

// Get returns the value stored under (id, key), or "" if unset.
func (l *localSidecar) Get(id, key string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.ensureLoadedLocked(); err != nil {
		return "", err
	}
	return l.data[id][key], nil
}

// DeleteBead removes all clone-local data for id. Callers should invoke this
// when the bead itself is deleted, so the sidecar doesn't accumulate entries
// for beads that no longer exist.
func (l *localSidecar) DeleteBead(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.ensureLoadedLocked(); err != nil {
		return err
	}
	if _, ok := l.data[id]; !ok {
		return nil
	}
	delete(l.data, id)
	return l.saveLocked()
}

func (l *localSidecar) saveLocked() error {
	if l.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(l.data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling local sidecar %q: %w", l.path, err)
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return fmt.Errorf("creating local sidecar dir for %q: %w", l.path, err)
	}
	return fsys.WriteFileAtomic(fsys.OSFS{}, l.path, raw, 0o644)
}
