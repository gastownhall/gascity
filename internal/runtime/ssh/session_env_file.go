package ssh

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Session environment reaches the box twice as process arguments: once as
// `tmux new-session ... -e KEY=VALUE`, and once as the `export KEY=VALUE`
// prelude embedded in the `sh -c ...` that carries PreStart / SessionSetup /
// SessionLive. Both land in a world-readable /proc/<pid>/cmdline — on the box
// AND in the local ssh client's own argv, since [sshArgs] folds the remote
// command into one argument. The tmux server holding the session outlives the
// session itself, so a credential passed that way is legible to every local
// user for the shell's whole life.
//
// stagedEnv is the fix: values classified secret by
// [runtime.SplitEnvByArgvSafety] are written to 0600 files inside a 0700
// directory on the box by a script delivered over ssh STDIN (never argv), and
// only paths are named on any command line. The sh file is sourced by the setup
// prelude; the tmux file carries the whole new-session command, run via
// `start-server ; source-file <path>` so tmux reads the values itself. The
// session environment tmux ends up holding is identical to the -e form, so
// show-environment readback still works.
type stagedEnv struct {
	dir      string // remote directory holding the files; "" when nothing was staged
	envPath  string // remote sh file the prelude sources; "" when nothing was staged
	tmuxPath string // remote tmux command file; "" when no session is being created
}

// staged reports whether anything was written to the box.
func (s stagedEnv) staged() bool { return s.dir != "" }

// stageSecretEnv writes the secret half of the session environment to private
// files on the box and returns their paths. secretEnv empty is the common
// no-op: nothing is written, no remote temp dir is created, and the caller keeps
// the plain argv path — so a box with no writable temp dir still starts sessions
// that carry no credentials.
//
// tmuxArgv, when non-empty, is the new-session argv to stage as a tmux command
// file. Relaunch passes nil: respawn-pane takes no -e and reuses the session
// environment already set at creation.
//
// A failure here is returned, never swallowed. Falling back to argv would put
// the credential right back on the command line, which is the leak this closes.
func (p *Provider) stageSecretEnv(ctx context.Context, secretEnv map[string]string, tmuxArgv []string) (stagedEnv, error) {
	if len(secretEnv) == 0 {
		return stagedEnv{}, nil
	}
	envBody := shellEnvFile(secretEnv)
	var tmuxBody string
	if len(tmuxArgv) > 0 {
		tmuxBody = tmuxCommandLine(tmuxArgv) + "\n"
	}
	script, err := stagingScript(envBody, tmuxBody)
	if err != nil {
		return stagedEnv{}, err
	}
	out, code, err := p.conn.execScript(ctx, []byte(script))
	if err != nil {
		return stagedEnv{}, fmt.Errorf("staging session env on box: %w", err)
	}
	// The answer must be an absolute path that looks like one of ours. It is
	// remote output and cleanupStagedEnv hands it to `rm -rf`, so a chatty
	// profile that writes AFTER our printf must not be able to name a directory
	// we then delete.
	dir := lastLine(string(out))
	if code != 0 || !strings.HasPrefix(dir, "/") || !strings.Contains(dir, "/"+stagedDirPrefix) {
		return stagedEnv{}, fmt.Errorf("staging session env on box: staging script exited %d without a staged directory path", code)
	}
	staged := stagedEnv{dir: dir, envPath: dir + "/" + envFileName}
	if tmuxBody != "" {
		staged.tmuxPath = dir + "/" + tmuxFileName
	}
	return staged, nil
}

// cleanupStagedEnvTimeout bounds the removal of the staged directory once the
// caller's own context is out of the picture.
const cleanupStagedEnvTimeout = 10 * time.Second

// cleanupStagedEnv removes the staged directory from the box. It deliberately
// outlives the caller's context: Start is most likely to bail out on a timeout
// or cancellation, which is exactly when the files still need removing. Still
// best-effort — the files are 0600 inside a 0700 directory, so a leftover after
// a dropped connection is readable only by the session's own user, and the next
// staging run on that box sweeps it (see [stagingScript]).
func (p *Provider) cleanupStagedEnv(ctx context.Context, name string, staged stagedEnv) {
	if !staged.staged() {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupStagedEnvTimeout)
	defer cancel()
	_, _, _ = p.conn.Exec(ctx, name, []string{"rm", "-rf", staged.dir})
}

// launchSession issues the new-session command, sourcing it from the staged
// tmux command file whenever one exists so that no environment value reaches a
// command line. source-file needs a server to talk to and — unlike new-session —
// cannot create one, so start-server runs first; it is a no-op when the box
// already has a tmux server on the socket.
func (p *Provider) launchSession(ctx context.Context, name string, args []string, staged stagedEnv) (string, int, error) {
	if staged.tmuxPath == "" {
		return p.tmux(ctx, name, args...)
	}
	return p.tmux(ctx, name, "start-server", ";", "source-file", staged.tmuxPath)
}

const (
	envFileName  = "env.sh"
	tmuxFileName = "session.tmux"
	// stagedDirPrefix names the private directories this package creates on the
	// box; the mktemp template and the sweep's glob are both derived from it so
	// the two cannot drift apart.
	stagedDirPrefix = "gc-session-"
	// staleStagedDirMinutes is how long a staged directory must have gone
	// untouched before the sweep treats it as orphaned. A live one exists only
	// for the duration of a single Start, so an hour is far outside any
	// legitimate lifetime and cannot race a concurrent starter.
	staleStagedDirMinutes = 60
)

// stagingScript builds the sh script that creates the private directory and
// writes the staged files. It is fed to a remote `sh` on stdin, so nothing it
// contains is ever visible in a command line. The heredoc delimiters are random,
// and the bodies are checked against them, so no value can terminate its own
// heredoc early.
//
// Every write is verified twice — `cat`'s exit status and the resulting byte
// count. An unverified heredoc is a silent failure mode with teeth: a full
// remote disk truncates env.sh mid-value, the truncated file still sources
// cleanly, and the session comes up missing exactly the credentials it needed.
//
// The sweep at the top is opportunistic litter collection for the crash case: a
// gc killed between mktemp and its cleanup leaves a secret-bearing directory on
// the box with nothing to remove it. Session staging is the only moment we know
// the box's temp dir is in use, so it is where the previous incarnation's litter
// gets collected. It is best-effort and its failure never blocks a session.
//
// Any failure after mktemp removes the directory before exiting, so a half-
// written credential never survives the run that failed to place it.
func stagingScript(envBody, tmuxBody string) (string, error) {
	envTag, err := heredocTag(envBody)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("umask 077\n")
	b.WriteString(`t="${TMPDIR:-/tmp}"` + "\n")
	fmt.Fprintf(&b, `find "$t" -maxdepth 1 -type d -name '%s*' -mmin +%d -exec rm -rf {} + 2>/dev/null`+"\n",
		stagedDirPrefix, staleStagedDirMinutes)
	fmt.Fprintf(&b, `d=$(mktemp -d "$t/%sXXXXXX") || exit 1`+"\n", stagedDirPrefix)
	b.WriteString(`fail() { rm -rf "$d"; exit 1; }` + "\n")
	writeFile := func(name, body, tag string) {
		b.WriteString(`cat > "$d/` + name + `" <<'` + tag + `' || fail` + "\n")
		b.WriteString(body)
		b.WriteString(tag + "\n")
		fmt.Fprintf(&b, `[ "$(wc -c < "$d/%s")" -eq %d ] || fail`+"\n", name, len(body))
	}
	writeFile(envFileName, envBody, envTag)
	if tmuxBody != "" {
		tmuxTag, err := heredocTag(tmuxBody)
		if err != nil {
			return "", err
		}
		writeFile(tmuxFileName, tmuxBody, tmuxTag)
	}
	// umask covers creation; chmod states the intent and repairs a box whose
	// mktemp or shell honored a looser mode.
	b.WriteString(`chmod 700 "$d" && chmod 600 "$d"/* || fail` + "\n")
	b.WriteString(`printf '%s\n' "$d"` + "\n")
	return b.String(), nil
}

// lastLine returns the final non-blank line of s, trimmed. The staging script
// prints the directory it created as its last line; anything a chatty remote
// profile wrote before that is not the answer.
func lastLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// heredocTag returns a random delimiter that does not occur in body.
func heredocTag(body string) (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		var raw [8]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", fmt.Errorf("generating heredoc delimiter: %w", err)
		}
		tag := "GC_EOF_" + strings.ToUpper(hex.EncodeToString(raw[:]))
		if !strings.Contains(body, tag) {
			return tag, nil
		}
	}
	return "", fmt.Errorf("generating heredoc delimiter: no delimiter free of the staged content")
}

// shellEnvFile renders env as a POSIX sh file that exports every entry when
// sourced. Keys are sorted so the file is deterministic.
func shellEnvFile(env map[string]string) string {
	var b strings.Builder
	for _, k := range sortedKeys(env) {
		b.WriteString(k + "=" + shellQuote([]string{env[k]}) + "\n")
		b.WriteString("export " + k + "\n")
	}
	return b.String()
}

// tmuxCommandLine renders a tmux argv as one line of a tmux command file.
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
// The local tmux provider carries its own copy (tmux.tmuxQuote), the way this
// package already carries its own shellQuote next to internal/shellquote:
// neither provider imports the other, and the runtime contract package stays
// free of provider syntax. Keep the two in step.
func tmuxQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
