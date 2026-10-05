package main

import (
	"errors"
	"path/filepath"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// The allocator's file and episode readers (P3 spec §4.2): provider health
// (I7), runtime suspension (I9), and startup-health episodes. A pass costs one
// stat per file and no store I/O on a non-exact leg: a file is re-read only
// when its modification time or size changes, and episodes come from the
// sessions leg's cache.

// statMemoMtimeGranularity bounds how coarse a filesystem's modification
// times may be. A load within this window of the file's mtime is racy: a
// same-size rewrite in the same mtime tick leaves the stat unchanged, so the
// memo reloads every read until a load lands after the window.
const statMemoMtimeGranularity = 2 * time.Second

// statMemo holds a value loaded from one file, reloading it only when the
// file's (existence, mtime, size) changes or the last load was racy. It is
// owned by one goroutine.
type statMemo[T any] struct {
	fs   fsys.FS
	path string
	load func() T
	now  func() time.Time // nil means time.Now

	loaded bool
	racy   bool
	key    statKey
	val    T
}

type statKey struct {
	exists bool
	mod    time.Time
	size   int64
}

// read stats the file and returns the memoized value, loading it first when
// the stat differs from the last load's or the last load was racy.
func (m *statMemo[T]) read() T {
	var key statKey
	if fi, err := m.fs.Stat(m.path); err == nil {
		key = statKey{exists: true, mod: fi.ModTime(), size: fi.Size()}
	}
	if !m.loaded || m.racy || key != m.key {
		now := time.Now
		if m.now != nil {
			now = m.now
		}
		m.racy = key.exists && now().Sub(key.mod) < statMemoMtimeGranularity
		m.val, m.key, m.loaded = m.load(), key, true
	}
	return m.val
}

// providerHealthReader is I7: provider-health.json, decoded once per change
// and evaluated at each pass's clock, so entries still age out between
// rewrites of the file.
type providerHealthReader struct {
	memo statMemo[*providerHealthFileFormat]
}

func newProviderHealthReader(fs fsys.FS, cityPath string) *providerHealthReader {
	path := filepath.Join(cityPath, providerHealthCacheRelPath)
	return &providerHealthReader{memo: statMemo[*providerHealthFileFormat]{fs: fs, path: path, load: func() *providerHealthFileFormat {
		data, err := fs.ReadFile(path)
		if err != nil {
			return nil
		}
		f, ok := parseProviderHealthFile(data)
		if !ok {
			return nil
		}
		return &f
	}}}
}

// snapshot returns the provider health at now. An absent or unreadable file
// reads as no registry, which fails open, as legacy does.
func (r *providerHealthReader) snapshot(now time.Time) *providerHealthSnapshot {
	f := r.memo.read()
	if f == nil {
		return &providerHealthSnapshot{present: false}
	}
	return f.snapshotAt(now)
}

// suspensionLoad is one read of the runtime suspension file.
type suspensionLoad struct {
	state suspensionstate.State
	err   error
}

// suspensionReader is I9: the runtime suspension state, which the allocator
// combines with config (effectiveCitySuspended).
type suspensionReader struct {
	memo statMemo[suspensionLoad]
}

func newSuspensionReader(fs fsys.FS, cityPath string) *suspensionReader {
	return &suspensionReader{memo: statMemo[suspensionLoad]{fs: fs, path: citylayout.SuspensionStateFile(cityPath), load: func() suspensionLoad {
		st, err := loadSuspensionState(fs, cityPath)
		return suspensionLoad{state: st, err: err}
	}}}
}

// state returns the runtime suspension state; an absent file is the zero
// state. A file that cannot be read or decoded returns its error until it
// changes.
func (r *suspensionReader) state() (suspensionstate.State, error) {
	l := r.memo.read()
	return l.state, l.err
}

// errEpisodesUncached reports a sessions store whose cache could not answer
// the episode read, with no last good within cacheLagBound. Consumers
// fail open on it (no episode), as legacy does on a load error
// (session_reconciler.go, LoadStartupHealthEpisode).
var errEpisodesUncached = errors.New("startup-health episodes: sessions cache unavailable")

// episodeReader reads the startup-health episodes (#46) from the sessions
// leg's CachingStore. An exact leg is read as the census reads it, through the
// cache's bounded dirty-row overlay; any other leg strict from memory, never
// from its backing store. A failed read serves the last good for
// cacheLagBound. It is owned by one goroutine.
type episodeReader struct {
	last map[string]session.StartupHealthEpisode
	at   time.Time
	ok   bool
}

// read returns the episodes keyed by their session name, which is the key
// legacy loads them by (startupHealthEpisodeKey(info, name)), and when they
// were read. A sessions store that is not a CachingStore, or whose read
// fails, serves the last good read within its bound; past it, or with none,
// read returns errEpisodesUncached.
func (r *episodeReader) read(store beads.Store, exact bool, now time.Time) (map[string]session.StartupHealthEpisode, time.Time, error) {
	base, _, _ := unwrapBeadPolicyStore(store)
	if cache, ok := base.(*beads.CachingStore); ok {
		q := beads.ListQuery{Type: session.StartupHealthEpisodeType}
		var rows []beads.Bead
		var err error
		if exact {
			rows, err = cache.List(q)
		} else if cached, clean := cache.CachedList(q); clean {
			rows = cached
		} else {
			err = errEpisodesUncached
		}
		if err == nil {
			// Legacy loads the first of ListByMetadata's order: the newest
			// created, ties by the largest bead ID.
			beads.SortBeads(rows, beads.SortCreatedDesc)
			episodes := make(map[string]session.StartupHealthEpisode, len(rows))
			for _, b := range rows {
				ep := session.StartupHealthEpisodeFromMetadata(b.Metadata)
				if _, dup := episodes[ep.SessionName]; !dup {
					episodes[ep.SessionName] = ep
				}
			}
			r.last, r.at, r.ok = episodes, now, true
		}
	}
	if !r.ok || now.Sub(r.at) > cacheLagBound {
		return nil, time.Time{}, errEpisodesUncached
	}
	return r.last, r.at, nil
}
