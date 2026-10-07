package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

var (
	sessionObjectIDRe = regexp.MustCompile(`^\$[0-9]+$`)
	decimalRe         = regexp.MustCompile(`^[0-9]+$`)
)

// validSessionObjectID reports whether id is a tmux #{session_id} ("$3").
func validSessionObjectID(id string) bool {
	return sessionObjectIDRe.MatchString(id)
}

// ServerConfirmedDead reports whether this provider's tmux server is
// confirmed dead: its socket is missing, or refuses connections on a stable
// inode, and the kernel lists no socket bound to the socket path. The last
// check refuses a live server whose socket file was unlinked, which the
// socket check alone reads as dead. It implements
// [runtime.ServerDeathConfirmer] for v2 only; legacy's serverConfirmedDead
// and ListRunning are unchanged.
func (p *Provider) ServerConfirmedDead() bool {
	if !p.tm.serverConfirmedDead() {
		return false
	}
	observe := p.unixListening
	if observe == nil {
		observe = unixPathListening
	}
	listening, err := observe(p.tm.serverSocketPath())
	return err == nil && !listening
}

// unixPathListening reports whether the kernel lists a unix socket bound to
// path (or to path under its resolved directory). Both tables keep a bound
// path after the file is unlinked: /proc/net/unix on Linux, where it counts
// only listeners, and lsof's bound addresses on macOS. Any other platform has
// no table and errors, so its server is never confirmed dead.
func unixPathListening(path string) (bool, error) {
	var listed func(path string) bool
	switch goruntime.GOOS {
	case "linux":
		table, err := os.ReadFile("/proc/net/unix")
		if err != nil {
			return false, err
		}
		listed = func(path string) bool { return procNetUnixListening(string(table), path) }
	case "darwin":
		out, err := lsofUnixSockets()
		if err != nil {
			return false, err
		}
		listed = func(path string) bool { return lsofUnixBound(out, path) }
	default:
		return false, fmt.Errorf("no unix socket table on %s", goruntime.GOOS)
	}
	paths := []string{path}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		paths = append(paths, filepath.Join(dir, filepath.Base(path)))
	}
	for _, candidate := range paths {
		if listed(candidate) {
			return true, nil
		}
	}
	return false, nil
}

// procNetUnixListening reports whether a /proc/net/unix table ("Num RefCount
// Protocol Flags Type St Inode Path") holds a socket bound to path with
// __SO_ACCEPTCON (0x10000) in Flags, the mark of a listener.
func procNetUnixListening(table, path string) bool {
	for _, line := range strings.Split(table, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 || strings.Join(fields[7:], " ") != path {
			continue
		}
		if flags, err := strconv.ParseUint(fields[3], 16, 64); err == nil && flags&0x10000 != 0 {
			return true
		}
	}
	return false
}

// lsofUnixSocketsTimeout bounds lsof's scan of every process it can read.
const lsofUnixSocketsTimeout = 10 * time.Second

// lsofUnixSockets returns lsof's field output ("n<name>" lines) for every
// unix socket it can read: the caller's own processes, which include any
// tmux server on the caller's sockets.
func lsofUnixSockets() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), lsofUnixSocketsTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "lsof", "-w", "-U", "-Fn")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	return lsofAnswer(out, stderr.String(), err)
}

// lsofAnswer reads an lsof run. lsof exits 1 both when it selects no file
// and when it fails; only a run that printed nothing at all is the empty
// answer. Any other failure, a timeout's kill included, is an error.
func lsofAnswer(out []byte, stderr string, err error) (string, error) {
	if err == nil {
		return string(out), nil
	}
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) && exit.ExitCode() == 1 && len(out) == 0 && stderr == "" {
		return "", nil
	}
	return "", fmt.Errorf("lsof -U: %w: %s", err, strings.TrimSpace(stderr))
}

// lsofUnixBound reports whether lsof field output names a unix socket bound
// to path. On macOS the name is the address the kernel holds for the socket,
// not a file lookup: a server's listener and its accepted connections keep
// it after the file is unlinked, while a client's socket names its peer
// ("->0x...") instead.
func lsofUnixBound(out, path string) bool {
	for _, line := range strings.Split(out, "\n") {
		if line == "n"+path {
			return true
		}
	}
	return false
}

// ObserveLivenessSince is ObserveLivenessWithError over a snapshot whose
// fetch started at or after since, on the cache clock (v5 O1 "fresh for the
// effect"). It reuses a new enough snapshot, refreshes otherwise, and never
// invalidates the cache. A refresh that fails for a missing server answers
// absent only when [Provider.ServerConfirmedDead] says so; any other failure
// is unknown. A running session with exactly one live pane also reports that
// pane's pid, for a zombie kill.
func (p *Provider) ObserveLivenessSince(name string, processNames []string, since time.Time) (runtime.Liveness, error) {
	if strings.TrimSpace(name) == "" {
		return runtime.Liveness{}, nil
	}
	obs, ok := p.cache.observeSince(since)
	if !ok {
		if isNoServerError(obs.lastErr) && p.ServerConfirmedDead() {
			return runtime.Liveness{}, nil
		}
		return runtime.Liveness{}, cacheUnknownError(name, obs.lastErr)
	}
	live := p.snapshotLiveness(obs.state, name, processNames)
	if session := obs.state.Sessions[name]; live.Running && len(session.Panes) == 1 && decimalRe.MatchString(session.Panes[0].PID) {
		live.PanePID = session.Panes[0].PID
	}
	return live, nil
}

// KillCorpseObject implements [runtime.SessionObjectKiller]: it kills
// objectID only while that session was created at created, is named name and
// has one window with one pane, and that pane is dead, checked by tmux in the
// same command as the kill. #{pane_dead} reads only the active pane, so a
// second pane, which may be live, refuses.
func (p *Provider) KillCorpseObject(name, objectID, created string) (runtime.SessionObjectKillResult, error) {
	if err := validateSessionObject(name, objectID, created); err != nil {
		return runtime.SessionObjectNotKilled, err
	}
	cond := fmt.Sprintf("#{&&:#{==:#{session_created},%s},#{&&:#{==:#{session_name},%s},#{&&:#{==:#{session_windows},1},#{&&:#{==:#{window_panes},1},#{pane_dead}}}}}", created, name)
	return p.killSessionObject(name, objectID, created, cond, runtime.SessionObjectLive)
}

// KillZombieObject implements [runtime.SessionObjectKiller]: it kills
// objectID only while that session was created at created, is named name and
// has one window with one live pane whose pid is panePID, checked by tmux in
// the same command as the kill.
func (p *Provider) KillZombieObject(name, objectID, created, panePID string) (runtime.SessionObjectKillResult, error) {
	if err := validateSessionObject(name, objectID, created); err != nil {
		return runtime.SessionObjectNotKilled, err
	}
	if !decimalRe.MatchString(panePID) {
		return runtime.SessionObjectNotKilled, fmt.Errorf("%w: pane pid %q must be decimal", runtime.ErrInvalidSessionObject, panePID)
	}
	cond := fmt.Sprintf("#{&&:#{==:#{session_created},%s},#{&&:#{==:#{session_name},%s},#{&&:#{==:#{session_windows},1},#{&&:#{==:#{window_panes},1},#{&&:#{!=:#{pane_dead},1},#{==:#{pane_pid},%s}}}}}}", created, name, panePID)
	return p.killSessionObject(name, objectID, created, cond, runtime.SessionObjectChanged)
}

// validateSessionObject keeps the values embedded in the tmux command to the
// session-name charset, a "$N" id and a decimal creation time (and, for a
// zombie, a decimal pid), so none can change the command or the format it
// runs.
func validateSessionObject(name, objectID, created string) error {
	if err := validateSessionName(name); err != nil {
		return fmt.Errorf("%w: %w", runtime.ErrInvalidSessionObject, err)
	}
	if !validSessionObjectID(objectID) {
		return fmt.Errorf("%w: session object id %q must match %s", runtime.ErrInvalidSessionObject, objectID, sessionObjectIDRe)
	}
	if !decimalRe.MatchString(created) {
		return fmt.Errorf("%w: session creation time %q must be decimal", runtime.ErrInvalidSessionObject, created)
	}
	return nil
}

// killSessionObject runs `if-shell -F -t <id> <cond> 'kill-session -t <id>'
// <report>` as one tmux command. The kill branch prints nothing; the refusal
// branch prints the session's creation time and name, so a refusal reads as
// gone (another object holds the id), a rename, or refused. tmux 3.4
// evaluates an id that names no session in an empty context: the condition,
// which requires a non-empty name, is false, and the report prints neither,
// which reads as gone (an older tmux that fails the target instead reads the
// same). The id is used verbatim and single-quoted, so tmux does not expand
// "$N" as a variable.
func (p *Provider) killSessionObject(name, objectID, created, cond string, refused runtime.SessionObjectKillResult) (runtime.SessionObjectKillResult, error) {
	report := fmt.Sprintf("display-message -t '%s' -p 'refused #{session_created} #{session_name}'", objectID)
	out, err := p.tm.run("if-shell", "-F", "-t", objectID, cond, fmt.Sprintf("kill-session -t '%s'", objectID), report)
	switch {
	case errors.Is(err, ErrSessionNotFound):
		return runtime.SessionObjectGone, nil
	case err != nil:
		return runtime.SessionObjectNotKilled, fmt.Errorf("killing tmux session object %s (%s): %w", objectID, name, err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		p.cache.EvictSession(name)
		return runtime.SessionObjectKilled, nil
	}
	if fields := strings.SplitN(out, " ", 3); fields[0] == "refused" {
		switch {
		case len(fields) < 2 || fields[1] != created:
			return runtime.SessionObjectGone, nil
		case len(fields) == 3 && fields[2] == name:
			return refused, nil
		}
		return runtime.SessionObjectRenamed, nil
	}
	return runtime.SessionObjectNotKilled, fmt.Errorf("killing tmux session object %s (%s): unexpected output %q", objectID, name, out)
}
