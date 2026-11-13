package main

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/config"
)

// workspaceEnvCache memoizes the RAW [workspace.env] table of a city's root
// city.toml, so the bd environment build learns the workspace's BD_BIN pin
// without a composed config load. The composed load resolves imports and packs
// and so takes the repo-cache lock; a `gc import install` clone holds that lock
// exclusively for its whole network fetch, which stalled every bd call on the
// machine behind it (#7170, ga-e0g3bz).
//
// [workspace.env] is a root-city.toml-only table: composition carries no env
// out of an included fragment, so the root file alone is the exact answer.
//
// Entries are keyed by the normalized city.toml path and validated on every
// hit by the size and mtime of city.toml alone. The table is stored raw:
// expandEnvMap reads the process environment, so an expanded value memoized
// here would go stale when the environment changes. Errors and a missing file
// are never cached.
var workspaceEnvCache sync.Map // normalized city.toml path → *workspaceEnvEntry

// workspaceEnvLoads counts real city.toml reads so tests can assert that
// repeated lookups reuse the entry.
var workspaceEnvLoads atomic.Int64

type workspaceEnvEntry struct {
	env   map[string]string
	stamp hostedCredentialProbeSource
}

// workspaceEnvDoc is the one table of city.toml this reader decodes.
type workspaceEnvDoc struct {
	Workspace config.Workspace `toml:"workspace"`
}

// resetWorkspaceEnvCache drops every memoized table (tests only).
func resetWorkspaceEnvCache() {
	workspaceEnvCache.Range(func(key, _ any) bool {
		workspaceEnvCache.Delete(key)
		return true
	})
}

// workspaceEnvTable returns the raw [workspace.env] table declared in
// cityPath's root city.toml, or nil when there is none. It reads that one file
// and never loads includes or packs, so it takes no repo-cache lock. The
// returned map is the caller's own copy. A missing city.toml is the empty
// answer.
func workspaceEnvTable(cityPath string) (map[string]string, error) {
	cityConfigPath := filepath.Join(cityPath, "city.toml")
	// The stamp reuses the hosted-credential probe's size+mtime source type.
	before, ok := statHostedCredentialProbeSource(cityConfigPath)
	if !ok {
		if _, err := os.Stat(cityConfigPath); errors.Is(err, os.ErrNotExist) {
			return nil, nil
		} else if err != nil {
			return nil, fmt.Errorf("read workspace env: %w", err)
		}
	}
	key := normalizePathForCompare(cityConfigPath)
	if v, loaded := workspaceEnvCache.Load(key); loaded {
		if entry, isEntry := v.(*workspaceEnvEntry); isEntry && entry.valid() {
			return maps.Clone(entry.env), nil
		}
		workspaceEnvCache.Delete(key)
	}
	data, err := os.ReadFile(cityConfigPath)
	workspaceEnvLoads.Add(1)
	if err != nil {
		return nil, fmt.Errorf("read workspace env: %w", err)
	}
	var doc workspaceEnvDoc
	if _, err := toml.Decode(string(data), &doc); err != nil {
		return nil, fmt.Errorf("decode workspace env in %s: %w", cityConfigPath, err)
	}
	// A city.toml rewritten while it was being read could leave an entry whose
	// stamp describes the new file but whose table came from the old one; skip
	// caching so the next call reads again.
	if after, ok := statHostedCredentialProbeSource(cityConfigPath); ok && after == before {
		workspaceEnvCache.Store(key, &workspaceEnvEntry{env: doc.Workspace.Env, stamp: before})
	}
	return maps.Clone(doc.Workspace.Env), nil
}

// valid reports whether city.toml still has the size and mtime the entry was
// read at.
func (e *workspaceEnvEntry) valid() bool {
	got, ok := statHostedCredentialProbeSource(e.stamp.path)
	return ok && got.size == e.stamp.size && got.modTime.Equal(e.stamp.modTime)
}
