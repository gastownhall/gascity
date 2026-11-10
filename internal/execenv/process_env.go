package execenv

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// The process environment has two views. While no projection window is open it
// is os.Environ(). While one is open, the open has rewritten the environment
// os.Environ() reports (it withholds some variables and projects others for the
// linked beads library to read), so the view every other reader gets is the
// snapshot taken as the window opened. Readers never wait: the window is
// entered with a context and is the only thing that can block, and only another
// window entry.
//
// projectionSeq is a sequence counter: odd while a window is open, even
// otherwise. A reader that finds it even reads os.Environ() and keeps the
// result only if the counter did not move meanwhile; otherwise it retries and
// finds the window's snapshot.
var (
	projectionSeq atomic.Uint64
	// projectionToken is a one-slot semaphore, full while no window is held.
	// A channel, not a mutex, so a waiting window entry can honor its context.
	projectionToken = func() chan struct{} {
		c := make(chan struct{}, 1)
		c <- struct{}{}
		return c
	}()
	// cleanEnvSnapshot is the ambient environment taken as the latest window
	// opened. It is read only while projectionSeq is odd, and is left in place
	// on exit: the next window replaces it before the counter turns odd.
	cleanEnvSnapshot atomic.Pointer[[]string]
)

// EnterProjectionWindow opens the one process-wide window in which a caller may
// rewrite this process's environment with os.Setenv and os.Unsetenv, and returns
// the function that closes it. It waits for another window to close, and gives
// up with an error wrapping ctx's when ctx ends first; a failed entry changes
// nothing. The caller must restore every variable it rewrote before calling
// exit, and should defer exit before the restore so a panic still releases the
// window. exit may be called more than once. Windows are not reentrant: a second
// entry from the holder's goroutine waits on itself until ctx ends.
//
// Inside the window, Environ, LookupEnv and Getenv keep answering with the
// environment as it was when the window opened, so a child process spawned or a
// variable read meanwhile never sees the projection.
func EnterProjectionWindow(ctx context.Context) (exit func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("entering the process environment projection window: %w", err)
	}
	select {
	case <-projectionToken:
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for the process environment projection window: %w", ctx.Err())
	}
	// select picks at random when both cases are ready: give the token back
	// rather than open a window for a caller whose context has already ended.
	if err := ctx.Err(); err != nil {
		projectionToken <- struct{}{}
		return nil, fmt.Errorf("entering the process environment projection window: %w", err)
	}
	snapshot := os.Environ()
	cleanEnvSnapshot.Store(&snapshot)
	projectionSeq.Add(1)
	return sync.OnceFunc(func() {
		projectionSeq.Add(1)
		projectionToken <- struct{}{}
	}), nil
}

// Environ returns a copy of this process's environment as it is outside any
// projection window: while one is open it is the environment as the window
// found it. It never waits. Build a child process's environment from it, never
// from a bare os.Environ(), in any process that may also open a native beads
// store.
func Environ() []string {
	for {
		seq := projectionSeq.Load()
		if seq%2 == 1 {
			return slices.Clone(*cleanEnvSnapshot.Load())
		}
		env := os.Environ()
		if projectionSeq.Load() == seq {
			return env
		}
	}
}

// LookupEnv is os.LookupEnv for the ambient environment: inside a projection
// window it answers from the snapshot taken as the window opened. It never
// waits.
func LookupEnv(key string) (string, bool) {
	for {
		seq := projectionSeq.Load()
		if seq%2 == 1 {
			return lookupSnapshot(*cleanEnvSnapshot.Load(), key)
		}
		value, ok := os.LookupEnv(key)
		if projectionSeq.Load() == seq {
			return value, ok
		}
	}
}

// Getenv is LookupEnv without the presence flag, like os.Getenv.
func Getenv(key string) string {
	value, _ := LookupEnv(key)
	return value
}

// lookupSnapshot finds key in a KEY=VALUE list the way os.LookupEnv finds it in
// the process environment: the first entry for a key wins, an entry with no "="
// is ignored, an empty key is never present, and a key containing "=" never
// matches. Names compare case-insensitively on Windows.
func lookupSnapshot(snapshot []string, key string) (string, bool) {
	if key == "" || strings.Contains(key, "=") {
		return "", false
	}
	for _, entry := range snapshot {
		// A name may start with "=" (Windows keeps per-drive directories that
		// way), so the separator is searched for from the second byte.
		sep := strings.IndexByte(entry[min(1, len(entry)):], '=')
		if sep < 0 {
			continue
		}
		name := entry[:sep+1]
		if name == key || (runtime.GOOS == "windows" && strings.EqualFold(name, key)) {
			return entry[sep+2:], true
		}
	}
	return "", false
}
