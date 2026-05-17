package main

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"sync"
	"time"
)

// SupervisorStatus is the subset of supervisor health/status information
// that the drift detector consumes. It is intentionally a small struct
// rather than a re-export of the full health response so that drift
// detection stays decoupled from the wire types.
type SupervisorStatus struct {
	// BuildID identifies the binary the running supervisor was built from.
	// Typically a short git commit hash. Empty when the supervisor binary
	// predates buildID exposure.
	BuildID string

	// PackRoots reports the supervisor's view of pack roots and when each
	// was last parsed. The drift detector compares ParsedAt to on-disk
	// mtime to determine whether the operator edited a pack since the
	// supervisor parsed it.
	PackRoots []PackRootStatus
}

// PackRootStatus describes a single pack root and the time the supervisor
// last parsed it.
type PackRootStatus struct {
	Dir      string
	ParsedAt time.Time
}

// SupervisorClient is the supervisor API surface required by drift
// detection. Implementations may be HTTP clients against the running
// supervisor's socket or test fakes.
type SupervisorClient interface {
	// Status returns the supervisor's reported status (build identity,
	// pack roots).
	Status(ctx context.Context) (SupervisorStatus, error)
	// Ping returns nil when the supervisor is responsive.
	Ping(ctx context.Context) error
}

// DetectBinaryDrift returns true when the locally-installed gc binary's
// build identity differs from the supervisor's reported build identity.
//
// Either side reporting an empty BuildID is treated as "unknown — cannot
// compare" and returns false. The caller is expected to fall back to a
// secondary signal (mtime comparison) when both buildIDs are absent.
func DetectBinaryDrift(localBuildID string, sv SupervisorStatus) bool {
	if localBuildID == "" || sv.BuildID == "" {
		return false
	}
	return localBuildID != sv.BuildID
}

// DetectPackDrift returns the directories whose newest file mtime is
// later than the supervisor's recorded ParsedAt, indicating the operator
// has edited a pack since the supervisor last parsed it.
//
// A pack root with a zero ParsedAt is skipped (no parse time to compare
// against). A missing directory is reported as an error so the caller
// can surface a clear message rather than silently treating it as
// no-drift.
func DetectPackDrift(packRoots []PackRootStatus) ([]string, error) {
	var drifted []string
	for _, root := range packRoots {
		if root.ParsedAt.IsZero() {
			continue
		}
		newest, err := walkNewestMtime(root.Dir)
		if err != nil {
			return nil, fmt.Errorf("pack root %q: %w", root.Dir, err)
		}
		if newest.After(root.ParsedAt) {
			drifted = append(drifted, root.Dir)
		}
	}
	return drifted, nil
}

// walkNewestMtime returns the newest mtime among regular files within
// dir. Directory mtimes are ignored. Returns an error if dir does not
// exist or cannot be walked.
func walkNewestMtime(dir string) (time.Time, error) {
	var newest time.Time
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest, err
}

// PollReady polls the supervisor's Ping endpoint until it returns nil
// or the timeout elapses. Returns the last error if the timeout is
// exceeded without a successful ping.
func PollReady(client SupervisorClient, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	var lastErr error
	for {
		err := client.Ping(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return fmt.Errorf("supervisor not ready within %s: %w", timeout, lastErr)
		}
		// Brief delay between pings — the supervisor is usually slow
		// enough to start that a short sleep avoids a busy loop without
		// noticeably extending the wait for the common case.
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return fmt.Errorf("supervisor not ready within %s: %w", timeout, lastErr)
		}
	}
}

// restartLoopGuard caps how many supervisor restarts may happen within
// a sliding window. The architecture's threshold is 3 restarts in 60s;
// a fourth within the window is refused so a misbehaving auto-restart
// never thrashes the system.
type restartLoopGuard struct {
	mu       sync.Mutex
	max      int
	window   time.Duration
	attempts []time.Time
}

func newRestartLoopGuard(maxAttempts int, window time.Duration) *restartLoopGuard {
	return &restartLoopGuard{max: maxAttempts, window: window}
}

// allowAt records a restart attempt at the given time and returns true
// if the attempt is within the configured budget. Once the budget is
// exhausted, returns false until enough attempts age out of the window.
func (g *restartLoopGuard) allowAt(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	cutoff := now.Add(-g.window)
	pruned := g.attempts[:0]
	for _, t := range g.attempts {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	g.attempts = pruned
	if len(g.attempts) >= g.max {
		return false
	}
	g.attempts = append(g.attempts, now)
	return true
}
