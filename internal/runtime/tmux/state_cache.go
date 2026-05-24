package tmux

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// defaultCacheTTL is the default time-to-live for cached session state.
const defaultCacheTTL = 2 * time.Second

// defaultStaleTTL is the maximum age of cached data before it is considered
// too stale to trust. After this duration, IsRunning returns false for all
// sessions and logs a degraded warning.
const defaultStaleTTL = 30 * time.Second

// fetchTimeout is the hard timeout for a single runtime-state fetch.
const fetchTimeout = 3 * time.Second

// StateFetcher abstracts tmux subprocess calls for testability.
type StateFetcher interface {
	// FetchState returns a runtime-state snapshot for live sessions.
	// Sessions with remain-on-exit corpses (pane_dead=1) are excluded.
	FetchState(ctx context.Context) (runtimeStateSnapshot, error)
}

type paneRuntimeState struct {
	Command string
	PID     string
}

type sessionRuntimeState struct {
	Running bool
	Panes   []paneRuntimeState
}

type processRuntimeState struct {
	PID     string
	PPID    string
	Command string
	Args    string
}

type processSnapshot struct {
	byPID    map[string]processRuntimeState
	children map[string][]string
}

type runtimeStateSnapshot struct {
	Sessions  map[string]sessionRuntimeState
	Processes processSnapshot
}

// StateCache caches tmux runtime state to avoid spawning N subprocess calls per
// status check or reconciler pass. Concurrent callers are coalesced via
// singleflight so at most one tmux/process snapshot refresh runs at a time.
type StateCache struct {
	mu         sync.RWMutex
	state      runtimeStateSnapshot
	fetchedAt  time.Time
	lastError  error
	dirty      bool   // set by Invalidate(); cleared on successful refresh
	generation uint64 // advanced by invalidation/eviction to reject stale refreshes
	ttl        time.Duration
	staleTTL   time.Duration
	sf         singleflight.Group
	fetcher    StateFetcher
}

// NewStateCache creates a new cache with the given fetcher and TTL.
// staleTTL defaults to 30s.
func NewStateCache(fetcher StateFetcher, ttl time.Duration) *StateCache {
	return &StateCache{
		fetcher:  fetcher,
		ttl:      ttl,
		staleTTL: defaultStaleTTL,
	}
}

// IsRunning reports whether the named session exists in the cached set.
// If the cache is stale, a refresh is triggered (coalesced via singleflight).
// On refresh failure, the last-known-good cache is preserved up to staleTTL.
func (c *StateCache) IsRunning(name string) bool {
	state := c.currentState()
	session, ok := state.Sessions[name]
	return ok && session.Running
}

// ProcessAlive reports whether the named session has a process matching one of
// processNames according to the cached runtime snapshot. An empty processNames
// slice preserves Provider.ProcessAlive's "no check possible" behavior.
func (c *StateCache) ProcessAlive(name string, processNames []string) bool {
	if len(processNames) == 0 {
		return true
	}
	return c.currentState().processAlive(name, processNames)
}

func (c *StateCache) currentState() runtimeStateSnapshot {
	c.mu.RLock()
	state := c.state
	fetchedAt := c.fetchedAt
	dirty := c.dirty
	c.mu.RUnlock()

	// Cache hit: fresh data, not invalidated.
	if state.Sessions != nil && !fetchedAt.IsZero() && !dirty && time.Since(fetchedAt) < c.ttl {
		return state
	}

	// Stale, empty, or dirty — trigger refresh.
	// When dirty, forget any in-flight singleflight so we get a fresh fetch
	// instead of coalescing with a pre-invalidation call.
	if dirty {
		c.sf.Forget("refresh")
	}
	c.refresh()

	// Read the (potentially updated) cache.
	c.mu.RLock()
	state = c.state
	fetchedAt = c.fetchedAt
	c.mu.RUnlock()

	// If the cache is older than staleTTL, report all sessions as not running.
	// Note: fetchedAt is preserved on failure (never zeroed), so this only
	// triggers after staleTTL of real wall-clock time since last success.
	if state.Sessions == nil || fetchedAt.IsZero() || time.Since(fetchedAt) > c.staleTTL {
		return runtimeStateSnapshot{}
	}
	return state
}

// Invalidate marks the cache as dirty, forcing the next IsRunning call
// to trigger a refresh. The session data and fetchedAt are preserved as
// last-known-good until the refresh completes — even if the refresh fails.
func (c *StateCache) Invalidate() {
	c.mu.Lock()
	c.dirty = true
	c.generation++
	c.mu.Unlock()
}

// EvictSession removes a specific session from the cache and marks it dirty.
// Used by Stop to immediately reflect the killed session without waiting for
// the next refresh cycle (which may race with singleflight coalescing).
func (c *StateCache) EvictSession(name string) {
	c.mu.Lock()
	delete(c.state.Sessions, name)
	c.dirty = true
	c.generation++
	c.mu.Unlock()
}

// refresh executes a single coalesced fetch. If the fetch fails, the
// last-known-good cache is preserved and the error is logged.
func (c *StateCache) refresh() {
	_, _, _ = c.sf.Do("refresh", func() (interface{}, error) {
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()

		c.mu.RLock()
		startGeneration := c.generation
		c.mu.RUnlock()

		start := time.Now()
		state, err := c.fetcher.FetchState(ctx)
		elapsed := time.Since(start)

		if err != nil {
			log.Printf("tmux state cache: refresh failed in %v: %v", elapsed, err)
			c.mu.Lock()
			c.lastError = err
			c.mu.Unlock()
			// Preserve last-known-good — do NOT update fetchedAt or sessions.
			return nil, err
		}

		c.mu.Lock()
		if c.generation != startGeneration {
			c.mu.Unlock()
			if os.Getenv("GC_LOG_TMUX_CACHE") == "true" {
				log.Printf("tmux state cache: discarded refresh from generation %d after %v", startGeneration, elapsed)
			}
			return nil, nil
		}
		// Successful refresh is noisy on the session loop; opt-in via env var
		// keeps it available for diagnostics without polluting normal CLI use.
		if os.Getenv("GC_LOG_TMUX_CACHE") == "true" {
			log.Printf("tmux state cache: refreshed %d sessions in %v", len(state.Sessions), elapsed)
		}

		c.state = state
		c.fetchedAt = time.Now()
		c.lastError = nil
		c.dirty = false
		c.mu.Unlock()
		return nil, nil
	})
}

// tmuxFetcher implements StateFetcher using a real Tmux instance.
type tmuxFetcher struct {
	tm *Tmux
}

// FetchState runs one tmux pane snapshot and one process-table snapshot.
// Sessions where remain-on-exit has kept a dead pane (pane_dead=1) are
// excluded — they represent exited processes, not running ones.
func (f *tmuxFetcher) FetchState(ctx context.Context) (runtimeStateSnapshot, error) {
	out, err := f.tm.runCtx(ctx, "list-panes", "-a", "-F", "#{session_name}\t#{pane_dead}\t#{pane_current_command}\t#{pane_pid}")
	if err != nil {
		if isNoServerError(err) {
			return runtimeStateSnapshot{Sessions: map[string]sessionRuntimeState{}}, nil // No server = no sessions
		}
		return runtimeStateSnapshot{}, err
	}
	state := runtimeStateSnapshot{
		Sessions: make(map[string]sessionRuntimeState),
	}
	if out == "" {
		return state, nil
	}

	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) < 2 || parts[0] == "" {
			continue
		}
		name := parts[0]
		if parts[1] == "1" {
			continue
		}
		var pane paneRuntimeState
		if len(parts) > 2 {
			pane.Command = strings.TrimSpace(parts[2])
		}
		if len(parts) > 3 {
			pane.PID = strings.TrimSpace(parts[3])
		}
		session := state.Sessions[name]
		session.Running = true
		if pane.Command != "" || pane.PID != "" {
			session.Panes = append(session.Panes, pane)
		}
		state.Sessions[name] = session
	}
	processes, err := fetchProcessSnapshot(ctx)
	if err != nil {
		return runtimeStateSnapshot{}, err
	}
	state.Processes = processes
	return state, nil
}

func (s runtimeStateSnapshot) processAlive(sessionName string, processNames []string) bool {
	session, ok := s.Sessions[sessionName]
	if !ok || !session.Running {
		return false
	}
	names := processNameSet(processNames)
	if len(names) == 0 {
		return false
	}
	for _, pane := range session.Panes {
		if pane.processAlive(names, s.Processes) {
			return true
		}
	}
	return false
}

func (p paneRuntimeState) processAlive(names map[string]struct{}, processes processSnapshot) bool {
	if _, ok := names[p.Command]; ok && p.Command != "" {
		return true
	}
	if p.PID == "" {
		return false
	}
	if isSupportedShell(p.Command) {
		return processes.hasDescendantWithNames(p.PID, names, 0)
	}
	if processes.processMatchesNames(p.PID, names) {
		return true
	}
	return processes.hasDescendantWithNames(p.PID, names, 0)
}

func processNameSet(names []string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			set[name] = struct{}{}
		}
	}
	return set
}

func processMatchesNameSet(command, args string, names map[string]struct{}) bool {
	if len(names) == 0 {
		return false
	}
	command = filepath.Base(strings.TrimSpace(command))
	if _, ok := names[command]; ok && command != "" {
		return true
	}
	argv := strings.Fields(strings.TrimSpace(args))
	if len(argv) == 0 {
		return false
	}
	argv0 := filepath.Base(argv[0])
	if _, ok := names[argv0]; ok {
		return true
	}
	if _, isInterpreter := knownInterpreters[argv0]; !isInterpreter {
		return false
	}
	for _, token := range argv[1:] {
		token = strings.TrimSpace(token)
		if token == "" || strings.HasPrefix(token, "-") {
			continue
		}
		if _, isRunner := runnerSubcommands[token]; isRunner {
			continue
		}
		base := filepath.Base(token)
		if _, ok := names[base]; ok {
			return true
		}
		baseNoExt := strings.TrimSuffix(base, filepath.Ext(base))
		if _, ok := names[baseNoExt]; ok {
			return true
		}
		break
	}
	return false
}

var knownInterpreters = map[string]struct{}{
	"node": {}, "bun": {}, "npx": {}, "deno": {},
}

var runnerSubcommands = map[string]struct{}{
	"run": {}, "exec": {}, "x": {},
}

const maxProcessDescendantDepth = 10

func isSupportedShell(command string) bool {
	for _, shell := range supportedShells {
		if command == shell {
			return true
		}
	}
	return false
}

func newProcessSnapshot(processes []processRuntimeState) processSnapshot {
	snapshot := processSnapshot{
		byPID:    make(map[string]processRuntimeState, len(processes)),
		children: make(map[string][]string),
	}
	for _, process := range processes {
		process.PID = strings.TrimSpace(process.PID)
		process.PPID = strings.TrimSpace(process.PPID)
		process.Command = strings.TrimSpace(process.Command)
		process.Args = strings.TrimSpace(process.Args)
		if process.PID == "" {
			continue
		}
		snapshot.byPID[process.PID] = process
		if process.PPID != "" {
			snapshot.children[process.PPID] = append(snapshot.children[process.PPID], process.PID)
		}
	}
	return snapshot
}

func fetchProcessSnapshot(ctx context.Context) (processSnapshot, error) {
	out, err := exec.CommandContext(ctx, "ps", processSnapshotPSArgs()...).Output()
	if err != nil {
		return processSnapshot{}, fmt.Errorf("fetching process snapshot: %w", err)
	}
	return parseProcessSnapshot(string(out)), nil
}

// processSnapshotPSArgs returns the platform-appropriate `ps` arguments for
// the process snapshot. macOS's ps does not accept the BSD column-width
// suffix (e.g. `pid:10=`) that Linux ps supports; on Darwin we omit the
// widths and parse with whitespace splitting. On Linux we keep the
// wide-column form so the fast fixed-column parser is exercised.
func processSnapshotPSArgs() []string {
	if goruntime.GOOS == "darwin" {
		return []string{"-eo", "pid=,ppid=,comm=,args="}
	}
	return []string{"-eo", "pid:10=,ppid:10=,comm:64=,args="}
}

func parseProcessSnapshot(out string) processSnapshot {
	processes := make([]processRuntimeState, 0)
	for _, line := range strings.Split(out, "\n") {
		process, ok := parseProcessSnapshotLine(line)
		if !ok {
			continue
		}
		processes = append(processes, process)
	}
	return newProcessSnapshot(processes)
}

func parseProcessSnapshotLine(line string) (processRuntimeState, bool) {
	if goruntime.GOOS == "darwin" {
		return parseProcessSnapshotLineWhitespace(line)
	}
	return parseProcessSnapshotLineFixedColumns(line)
}

func parseProcessSnapshotLineFixedColumns(line string) (processRuntimeState, bool) {
	const (
		pidWidth     = 10
		ppidStart    = pidWidth + 1
		ppidWidth    = 10
		commandStart = ppidStart + ppidWidth + 1
		commandWidth = 64
		argsStart    = commandStart + commandWidth + 1
	)
	if len(line) < commandStart+commandWidth {
		return processRuntimeState{}, false
	}
	process := processRuntimeState{
		PID:     strings.TrimSpace(line[:pidWidth]),
		PPID:    strings.TrimSpace(line[ppidStart : ppidStart+ppidWidth]),
		Command: strings.TrimSpace(line[commandStart : commandStart+commandWidth]),
	}
	if len(line) > argsStart {
		process.Args = strings.TrimSpace(line[argsStart:])
	}
	if process.PID == "" || process.PPID == "" || process.Command == "" {
		return processRuntimeState{}, false
	}
	return process, true
}

// parseProcessSnapshotLineWhitespace parses a single line of
// `ps -eo pid=,ppid=,comm=,args=` output. Format: 3 whitespace-separated
// fixed tokens (pid, ppid, comm) followed by args. comm is the command
// name without path/args, typically a single token. Anything from the
// fourth-token boundary onward is treated as args verbatim (preserving
// internal whitespace) so callers can match against the full command line.
func parseProcessSnapshotLineWhitespace(line string) (processRuntimeState, bool) {
	trimmed := strings.TrimLeft(line, " \t")
	remaining := trimmed
	for i := 0; i < 3; i++ {
		end := strings.IndexAny(remaining, " \t")
		if end < 0 {
			// Not enough fields on this line. Lines with fewer than 4
			// tokens (e.g. kernel threads with no args) still need to
			// match pid/ppid/comm — fall through to the Fields parse.
			break
		}
		remaining = strings.TrimLeft(remaining[end:], " \t")
	}
	fields := strings.Fields(trimmed)
	if len(fields) < 3 {
		return processRuntimeState{}, false
	}
	process := processRuntimeState{
		PID:     fields[0],
		PPID:    fields[1],
		Command: fields[2],
	}
	if process.PID == "" || process.PPID == "" || process.Command == "" {
		return processRuntimeState{}, false
	}
	if len(fields) > 3 {
		// `remaining` now points past the third token boundary; args
		// preserves the original spacing between tokens 4..N.
		process.Args = strings.TrimSpace(remaining)
	}
	return process, true
}

func (s processSnapshot) processMatchesNames(pid string, names map[string]struct{}) bool {
	process, ok := s.byPID[pid]
	if !ok {
		return false
	}
	return processMatchesNameSet(process.Command, process.Args, names)
}

func (s processSnapshot) hasDescendantWithNames(pid string, names map[string]struct{}, depth int) bool {
	if len(names) == 0 || depth > maxProcessDescendantDepth {
		return false
	}
	for _, childPID := range s.children[pid] {
		if s.processMatchesNames(childPID, names) {
			return true
		}
		if s.hasDescendantWithNames(childPID, names, depth+1) {
			return true
		}
	}
	return false
}

// isNoServerError checks if the error is a "no server running" error.
func isNoServerError(err error) bool {
	return errors.Is(err, ErrNoServer) || (err != nil && strings.Contains(err.Error(), "no server running"))
}

// cacheTTLFromEnv reads GC_TMUX_CACHE_TTL from the environment and parses
// it as a duration. Returns defaultCacheTTL if the env var is unset, empty,
// or cannot be parsed. Accepts:
//   - integer: interpreted as milliseconds (e.g., "2000" = 2s)
//   - Go duration string: (e.g., "2s", "500ms")
func cacheTTLFromEnv() time.Duration {
	v := os.Getenv("GC_TMUX_CACHE_TTL")
	if v == "" {
		return defaultCacheTTL
	}

	// Try Go duration string first (e.g., "2s", "500ms").
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}

	// Try integer milliseconds (e.g., "2000").
	if strings.TrimSpace(v) == v {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}

	log.Printf("tmux state cache: invalid GC_TMUX_CACHE_TTL=%q, using default %v", v, defaultCacheTTL)
	return defaultCacheTTL
}
