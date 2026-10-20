package tmux

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

// boxEnvExecutor models the parts of one tmux server that decide what a
// respawned pane inherits: the server's global environment, one session's
// environment (values plus `-r` removal markers), and respawn-pane building
// the child's environment from the two. respawn-pane itself takes no env from
// the caller, which is the whole point: whatever the relaunch wants the agent to
// see has to be in the session environment first.
type boxEnvExecutor struct {
	global  map[string]string
	session map[string]string
	removed map[string]bool

	calls      [][]string // top-level calls, as the caller passed them
	respawned  bool
	respawnEnv map[string]string // what the respawned process starts with

	showErr error // returned by show-environment when set
}

func newBoxEnvExecutor(global, session map[string]string, removed ...string) *boxEnvExecutor {
	b := &boxEnvExecutor{
		global:  map[string]string{},
		session: map[string]string{},
		removed: map[string]bool{},
	}
	for k, v := range global {
		b.global[k] = v
	}
	for k, v := range session {
		b.session[k] = v
	}
	for _, k := range removed {
		b.removed[k] = true
	}
	return b
}

func (b *boxEnvExecutor) execute(args []string) (string, error) {
	cp := make([]string, len(args))
	copy(cp, args)
	b.calls = append(b.calls, cp)
	return b.apply(args)
}

func (b *boxEnvExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return b.execute(args)
}

func (b *boxEnvExecutor) apply(args []string) (string, error) {
	// runCtx prepends "-u" (and -L when socketed), so skip to the subcommand.
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		if args[i] == "-L" {
			i++
		}
		i++
	}
	if i >= len(args) {
		return "", nil
	}
	sub, rest := args[i], args[i+1:]
	switch sub {
	case "show-environment":
		if b.showErr != nil {
			return "", b.showErr
		}
		var lines []string
		for k, v := range b.session {
			lines = append(lines, k+"="+v)
		}
		for k := range b.removed {
			lines = append(lines, "-"+k)
		}
		slices.Sort(lines)
		return strings.Join(lines, "\n"), nil
	case "set-environment":
		rest = dropTarget(rest)
		switch {
		case len(rest) == 2 && rest[0] == "-r":
			delete(b.session, rest[1])
			b.removed[rest[1]] = true
		case len(rest) == 2 && rest[0] == "-u":
			delete(b.session, rest[1])
			delete(b.removed, rest[1])
		case len(rest) == 2:
			b.session[rest[0]] = rest[1]
			delete(b.removed, rest[0])
		default:
			return "", errors.New("fake tmux: unexpected set-environment form")
		}
		return "", nil
	case "source-file":
		data, err := os.ReadFile(rest[len(rest)-1])
		if err != nil {
			return "", err
		}
		return b.apply(parseTmuxCommandLine(strings.TrimSpace(string(data))))
	case "list-panes":
		// The box has one pane, already marked as the agent's: the adopt
		// check leaves it alone and the pane lookup resolves to it.
		return "agent\t@1.%0", nil
	case "respawn-pane":
		env := map[string]string{}
		for k, v := range b.global {
			env[k] = v
		}
		for k := range b.removed {
			delete(env, k)
		}
		for k, v := range b.session {
			env[k] = v
		}
		b.respawned = true
		b.respawnEnv = env
		return "", nil
	}
	return "", nil
}

func dropTarget(args []string) []string {
	if len(args) >= 2 && args[0] == "-t" {
		return args[2:]
	}
	return args
}

// parseTmuxCommandLine reverses tmuxCommandLine: single-quoted words, with an
// embedded quote spliced in as a backslash-escaped quote between quoted runs.
func parseTmuxCommandLine(line string) []string {
	var words []string
	var cur strings.Builder
	inWord, inQuote := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inQuote && c == '\'':
			inQuote = false
		case inQuote:
			cur.WriteByte(c)
		case c == '\'':
			inQuote, inWord = true, true
		case c == '\\' && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
			inWord = true
		case c == ' ':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

func relaunchIn(t *testing.T, box *boxEnvExecutor, env map[string]string) error {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	tm := NewTmux()
	tm.exec = box
	ops := &tmuxStartOps{tm: tm}
	return ops.respawnAgent("gc-box", "/proj", "claude", env)
}

func (b *boxEnvExecutor) setEnvCalls() []string {
	var out []string
	for _, c := range b.calls {
		joined := strings.Join(c, " ")
		if strings.Contains(joined, "set-environment") || strings.Contains(joined, "source-file") {
			out = append(out, joined)
		}
	}
	return out
}

// An operator env key added after the box was provisioned (a provider env
// edit, say) is the reason the agent is being relaunched. The respawned agent
// has to see it.
func TestRespawnAgentDeliversLaunchEnvKeyAddedSinceProvision(t *testing.T) {
	box := newBoxEnvExecutor(nil, map[string]string{
		"GC_CITY":           "/city",
		launchEnvKeysEnvKey: "GC_CITY",
	})
	err := relaunchIn(t, box, map[string]string{
		"GC_CITY":                   "/city",
		"BD_PRIME_MAX_MEMORY_CHARS": "2000",
	})
	if err != nil {
		t.Fatalf("respawnAgent: %v", err)
	}
	if !box.respawned {
		t.Fatal("respawn-pane never ran")
	}
	if got := box.respawnEnv["BD_PRIME_MAX_MEMORY_CHARS"]; got != "2000" {
		t.Fatalf("respawned agent BD_PRIME_MAX_MEMORY_CHARS = %q, want 2000; the added key never reached it", got)
	}
	if got, want := box.session[launchEnvKeysEnvKey], "BD_PRIME_MAX_MEMORY_CHARS,GC_CITY"; got != want {
		t.Fatalf("recorded launch keys = %q, want %q", got, want)
	}
}

func TestRespawnAgentDeliversChangedLaunchEnvValue(t *testing.T) {
	box := newBoxEnvExecutor(map[string]string{"ANTHROPIC_MODEL": "server-default"}, map[string]string{
		"ANTHROPIC_MODEL":   "old-model",
		"GC_CITY":           "/city",
		launchEnvKeysEnvKey: "ANTHROPIC_MODEL,GC_CITY",
	})
	err := relaunchIn(t, box, map[string]string{
		"ANTHROPIC_MODEL": "new-model",
		"GC_CITY":         "/city",
	})
	if err != nil {
		t.Fatalf("respawnAgent: %v", err)
	}
	if got := box.respawnEnv["ANTHROPIC_MODEL"]; got != "new-model" {
		t.Fatalf("respawned agent ANTHROPIC_MODEL = %q, want new-model", got)
	}
}

// A key the last launch set and this launch no longer carries must not reach
// the respawned agent, while session metadata gc keeps in the same environment
// (drain markers and the like, never part of a launch env) must survive.
func TestRespawnAgentDropsLaunchEnvKeyRemovedSinceLastLaunch(t *testing.T) {
	box := newBoxEnvExecutor(nil, map[string]string{
		"GC_CITY":           "/city",
		"RETIRED_SETTING":   "1",
		"GC_DRAIN":          "1792342852",
		launchEnvKeysEnvKey: "GC_CITY,RETIRED_SETTING",
	})
	err := relaunchIn(t, box, map[string]string{"GC_CITY": "/city"})
	if err != nil {
		t.Fatalf("respawnAgent: %v", err)
	}
	if v, ok := box.respawnEnv["RETIRED_SETTING"]; ok {
		t.Fatalf("respawned agent still has RETIRED_SETTING=%q after it left the launch env", v)
	}
	if got := box.session["GC_DRAIN"]; got != "1792342852" {
		t.Fatalf("session metadata GC_DRAIN = %q after relaunch, want it untouched", got)
	}
	if got, want := box.session[launchEnvKeysEnvKey], "GC_CITY"; got != want {
		t.Fatalf("recorded launch keys = %q, want %q", got, want)
	}
}

// A launch key whose value is now empty is withheld, so a value the box still
// holds from an earlier launch must not reach the respawned agent either.
func TestRespawnAgentDropsLaunchEnvKeyNowEmpty(t *testing.T) {
	box := newBoxEnvExecutor(nil, map[string]string{
		"GC_CITY":           "/city",
		"CLAUDECODE":        "1",
		launchEnvKeysEnvKey: "CLAUDECODE,GC_CITY",
	})
	err := relaunchIn(t, box, map[string]string{"GC_CITY": "/city", "CLAUDECODE": ""})
	if err != nil {
		t.Fatalf("respawnAgent: %v", err)
	}
	if v, ok := box.respawnEnv["CLAUDECODE"]; ok {
		t.Fatalf("respawned agent has CLAUDECODE=%q, want it withheld", v)
	}
}

// A withheld controller-scope key stays withheld through the env sync: it is
// marked removed (-r), never merely unset (-u), even when the last launch had
// recorded it as a key it set.
func TestRespawnAgentKeepsControllerKeyWithheldThroughEnvSync(t *testing.T) {
	for name, box := range map[string]*boxEnvExecutor{
		"already marked": newBoxEnvExecutor(
			map[string]string{"GC_CONTROLLER_TOKEN": "controller-secret"},
			map[string]string{"GC_CITY": "/city", launchEnvKeysEnvKey: "GC_CITY"},
			"GC_CONTROLLER_TOKEN"),
		"recorded as set by the last launch": newBoxEnvExecutor(
			map[string]string{"GC_CONTROLLER_TOKEN": "controller-secret"},
			map[string]string{"GC_CITY": "/city", "GC_CONTROLLER_TOKEN": "stale", launchEnvKeysEnvKey: "GC_CITY,GC_CONTROLLER_TOKEN"}),
	} {
		t.Run(name, func(t *testing.T) {
			err := relaunchIn(t, box, map[string]string{"GC_CITY": "/city", "GC_CONTROLLER_TOKEN": ""})
			if err != nil {
				t.Fatalf("respawnAgent: %v", err)
			}
			if v, ok := box.respawnEnv["GC_CONTROLLER_TOKEN"]; ok {
				t.Fatalf("respawned agent has GC_CONTROLLER_TOKEN=%q, want it withheld", v)
			}
			if !box.removed["GC_CONTROLLER_TOKEN"] {
				t.Fatalf("GC_CONTROLLER_TOKEN is not marked removed in the session env after relaunch: %v", box.setEnvCalls())
			}
		})
	}
}

// A value that is not argv-safe reaches the session through the private
// command file, never as a set-environment argument.
func TestRespawnAgentSetsSecretLaunchEnvOutsideArgv(t *testing.T) {
	const canary = "relaunch-secret-canary"
	box := newBoxEnvExecutor(nil, map[string]string{"GC_CITY": "/city", launchEnvKeysEnvKey: "GC_CITY"})
	err := relaunchIn(t, box, map[string]string{"GC_CITY": "/city", "ANTHROPIC_AUTH_TOKEN": canary})
	if err != nil {
		t.Fatalf("respawnAgent: %v", err)
	}
	if got := box.respawnEnv["ANTHROPIC_AUTH_TOKEN"]; got != canary {
		t.Fatalf("respawned agent ANTHROPIC_AUTH_TOKEN = %q, want the new value", got)
	}
	for _, c := range box.calls {
		if slices.Contains(c, canary) || strings.Contains(strings.Join(c, " "), canary) {
			t.Fatalf("secret value reached tmux argv: %v", c)
		}
	}
}

// A box provisioned before the launch keys were recorded cannot say which of
// its entries a launch set, so nothing is removed from it; added and changed
// keys are still delivered, and the record is written for the next relaunch.
func TestRespawnAgentOnUnrecordedBoxDeliversButRemovesNothing(t *testing.T) {
	box := newBoxEnvExecutor(nil, map[string]string{
		"GC_CITY":         "/city",
		"RETIRED_SETTING": "1",
	})
	err := relaunchIn(t, box, map[string]string{"GC_CITY": "/city", "NEW_SETTING": "yes"})
	if err != nil {
		t.Fatalf("respawnAgent: %v", err)
	}
	if got := box.respawnEnv["NEW_SETTING"]; got != "yes" {
		t.Fatalf("respawned agent NEW_SETTING = %q, want yes", got)
	}
	if got := box.session["RETIRED_SETTING"]; got != "1" {
		t.Fatalf("RETIRED_SETTING = %q on an unrecorded box, want it left alone", got)
	}
	if got, want := box.session[launchEnvKeysEnvKey], "GC_CITY,NEW_SETTING"; got != want {
		t.Fatalf("recorded launch keys = %q, want %q", got, want)
	}
}

// A relaunch whose env already matches the box writes nothing to it.
func TestRespawnAgentWritesNothingWhenBoxEnvMatches(t *testing.T) {
	box := newBoxEnvExecutor(nil, map[string]string{
		"GC_CITY":           "/city",
		"LANG":              "en_US.UTF-8",
		launchEnvKeysEnvKey: "GC_CITY,LANG",
	})
	err := relaunchIn(t, box, map[string]string{"GC_CITY": "/city", "LANG": "en_US.UTF-8", "CLAUDECODE": ""})
	if err != nil {
		t.Fatalf("respawnAgent: %v", err)
	}
	if calls := box.setEnvCalls(); len(calls) != 0 {
		t.Fatalf("set-environment calls = %v, want none for an unchanged env", calls)
	}
	if !box.respawned {
		t.Fatal("respawn-pane never ran")
	}
}

// If the box's environment cannot be read, the relaunch fails before the
// respawn rather than starting an agent with an environment it cannot vouch
// for; the caller then falls back to a full restart.
func TestRespawnAgentFailsWhenBoxEnvUnreadable(t *testing.T) {
	box := newBoxEnvExecutor(nil, nil)
	box.showErr = errors.New("exit status 1")
	err := relaunchIn(t, box, map[string]string{"GC_CITY": "/city"})
	if err == nil {
		t.Fatal("respawnAgent = nil, want the env read failure")
	}
	if box.respawned {
		t.Fatal("respawn-pane ran after the env read failed")
	}
}

// The create path records the keys its env sets, so the first relaunch of a
// new box can tell an operator's removal from gc's own session metadata.
func TestCreateSessionRecordsLaunchEnvKeys(t *testing.T) {
	exec := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = exec
	ops := &tmuxStartOps{tm: tm}

	if err := ops.createSession("gc-box", "", "claude", map[string]string{
		"GC_CITY":             "/city",
		"LANG":                "en_US.UTF-8",
		"GC_CONTROLLER_TOKEN": "",
	}); err != nil {
		t.Fatalf("createSession: %v", err)
	}
	newSession := newSessionCall(t, exec.calls)
	joined := strings.Join(newSession, "\x00")
	if !strings.Contains(joined, "\x00-e\x00"+launchEnvKeysEnvKey+"=GC_CITY,LANG\x00") {
		t.Fatalf("new-session args do not record the launch keys: %v", newSession)
	}

	exec = &fakeExecutor{}
	tm.exec = exec
	if err := ops.createSession("gc-box", "", "claude", map[string]string{"CLAUDECODE": ""}); err != nil {
		t.Fatalf("createSession: %v", err)
	}
	for _, c := range exec.calls {
		if joined := strings.Join(c, " "); strings.Contains(joined, launchEnvKeysEnvKey) {
			t.Fatalf("tmux call recorded launch keys for an env that sets none: %v", c)
		}
	}
}

func newSessionCall(t *testing.T, calls [][]string) []string {
	t.Helper()
	for _, c := range calls {
		if slices.Contains(c, "new-session") {
			return c
		}
	}
	t.Fatalf("no new-session call in %v", calls)
	return nil
}
