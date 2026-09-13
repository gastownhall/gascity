package tmux

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// tmux takes session environment as `-e KEY=VALUE` arguments. Those arguments
// land in /proc/<pid>/cmdline of the tmux client AND of the tmux server it
// spawns — world-readable (0444), and the server lives as long as the session
// does. Any local user could read every agent credential off a box for the
// lifetime of the shell.
//
// So when env carries a secret, the entire command is written to a private
// (0600, inside a 0700 directory) tmux command file and executed with
// `start-server ; source-file <path>`: only the path appears in argv, and tmux
// reads the values itself. start-server comes first because source-file needs a
// server to talk to and, unlike new-session, cannot create one. The resulting
// session environment is byte-identical to the argv form (verified against tmux
// 3.4 for values holding quotes, `$`, `#{}`, `;`, backslashes and newlines), so
// show-environment readback and pane-respawn inheritance are unaffected, and a
// failure inside the file — a duplicate session, above all — still reaches
// wrapError with tmux's own stderr, so the ErrSessionExists sentinel survives.
const (
	// stagedDirPrefix names the private directory each staged command file
	// lives in. The directory, not just the file, is what the sweep collects.
	stagedDirPrefix = "gc-tmux-session-"
	stagedFileName  = "session.tmux"

	// staleStagedDirAge is how long a staged directory must have gone untouched
	// before the sweep treats it as orphaned. A live one exists only for the
	// duration of a single source-file call — milliseconds — so an hour is far
	// outside any legitimate lifetime and cannot race a concurrent starter.
	staleStagedDirAge = time.Hour
)

// runNewSession issues a prepared new-session argv, keeping secret environment
// values out of the process argument vector.
//
// An all-inert environment keeps the plain argv path: it needs no writable temp
// dir, so the common case cannot fail on a read-only filesystem. When a secret
// IS present and the file cannot be written, this fails closed rather than
// falling back to argv — a silent fallback is exactly the leak we are closing.
func (t *Tmux) runNewSession(args []string, env map[string]string) error {
	if !runtime.EnvHasArgvSecrets(env) {
		_, err := t.run(args...)
		return err
	}
	// Opportunistic, best-effort: a process killed between MkdirTemp and its
	// deferred cleanup leaves a secret-bearing file behind with nothing to
	// remove it. Session creation is the only moment we know the staging
	// directory is in use, so it is where the previous incarnation's litter gets
	// collected.
	sweepStaleStagedDirs()

	path, cleanup, err := stageTmuxCommandFile(tmuxCommandLine(args))
	if err != nil {
		return fmt.Errorf("staging tmux command file (session env holds secrets that must not reach argv): %w", err)
	}
	defer cleanup()
	_, err = t.run("start-server", ";", "source-file", path)
	return err
}

// stageTmuxCommandFile writes line to a private file and returns its path plus a
// cleanup func that removes it. The file is created O_EXCL inside a directory
// this call just made, and both are chmod'ed explicitly, so neither a permissive
// umask nor a pre-existing symlink can widen or hijack them. Its lifetime is the
// source-file call, not the session's — the caller removes it as soon as tmux
// has read it.
func stageTmuxCommandFile(line string) (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", stagedDirPrefix+"*")
	if err != nil {
		return "", nil, err
	}
	remove := func() { _ = os.RemoveAll(dir) }
	// MkdirTemp already creates 0700, but umask can only clear bits and a future
	// change to the helper should not be able to widen this silently.
	if err := os.Chmod(dir, 0o700); err != nil {
		remove()
		return "", nil, err
	}
	path = filepath.Join(dir, stagedFileName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		remove()
		return "", nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		remove()
		return "", nil, err
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		_ = f.Close()
		remove()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		remove()
		return "", nil, err
	}
	return path, remove, nil
}

// sweepStaleStagedDirs removes staged directories older than staleStagedDirAge
// from the temp dir. Every error is ignored on purpose: this is litter
// collection, not a control — a directory another user owns is not ours to
// remove (and is not ours to leak either), and a failure here must never stop a
// session from starting.
func sweepStaleStagedDirs() {
	tmp := os.TempDir()
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-staleStagedDirAge)
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), stagedDirPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(tmp, e.Name()))
	}
}

// tmuxCommandLine renders a tmux argv as a single line of a tmux command file.
func tmuxCommandLine(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = tmuxQuote(a)
	}
	return strings.Join(quoted, " ")
}

// tmuxQuote renders s as one literal argument in tmux's config syntax.
//
// tmux takes everything inside single quotes literally — no escapes, no `$` or
// `#{}` expansion, and embedded newlines are fine — but offers no way to put a
// single quote inside them. Splice those in as `\'` between quoted runs, the
// same shape POSIX shells use.
//
// The ssh provider carries its own copy for the box-side file; neither provider
// imports the other. Keep the two in step.
func tmuxQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
