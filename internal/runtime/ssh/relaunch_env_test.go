package ssh

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// envBox models the parts of a remote tmux server that decide what a
// respawned agent inherits: the server's global environment, one session's
// environment (values plus -r removal marks), the session's launch-key
// record, and respawn-pane building the child's environment from them.
// respawn-pane takes no env from the caller, which is the point: whatever a
// relaunch wants the agent to see has to be in the session environment first.
type envBox struct {
	f *fakeRunner

	global  map[string]string
	session map[string]string
	removed map[string]bool
	record  string
	created bool

	staged     string // body of the last staged tmux command file
	respawned  bool
	respawnEnv map[string]string

	showFails bool // show-environment exits 1
}

func newEnvBox(global map[string]string) *envBox {
	b := &envBox{
		global:  maps.Clone(global),
		session: map[string]string{},
		removed: map[string]bool{},
	}
	if b.global == nil {
		b.global = map[string]string{}
	}
	b.f = &fakeRunner{respond: b.respond}
	return b
}

// warm makes the box hold a session created before the launch-key record
// existed, with session env env.
func (b *envBox) warm(env map[string]string) {
	b.created = true
	maps.Copy(b.session, env)
}

func (b *envBox) respond(argv []string) ([]byte, int, error) {
	switch {
	case isStagingScript(argv):
		b.staged = stagedTmuxBody(string(b.f.stdins[len(b.f.stdins)-1]))
		return []byte(stagedDir + "\n"), 0, nil
	case len(argv) > 0 && argv[0] == "tmux":
		out, code := b.tmux(argv[1:], false)
		return []byte(out), code, nil
	}
	return nil, 0, nil
}

// stagedTmuxBody extracts the session.tmux heredoc from a staging script.
func stagedTmuxBody(script string) string {
	const marker = `/` + tmuxFileName + `" <<'`
	i := strings.Index(script, marker)
	if i < 0 {
		return ""
	}
	tag, rest, _ := strings.Cut(script[i+len(marker):], "'")
	_, rest, _ = strings.Cut(rest, "\n") // the rest of the cat line
	body, _, _ := strings.Cut(rest, tag+"\n")
	return body
}

// tmux runs a tmux command sequence: ";" arguments separate commands. As
// argv, an argument ending in an unescaped ";" also ends its command and a
// trailing "\;" is a literal ";"; in a command file (fromFile) every argument
// is quoted and taken literally. The sequence stops at the first failing
// command.
func (b *envBox) tmux(args []string, fromFile bool) (string, int) {
	var cmds [][]string
	var cur []string
	for _, a := range args {
		switch {
		case a == ";":
			cmds, cur = append(cmds, cur), nil
		case fromFile:
			cur = append(cur, a)
		case strings.HasSuffix(a, `\;`):
			cur = append(cur, a[:len(a)-2]+";")
		case strings.HasSuffix(a, ";"):
			cmds, cur = append(cmds, append(cur, a[:len(a)-1])), nil
		default:
			cur = append(cur, a)
		}
	}
	cmds = append(cmds, cur)
	var out strings.Builder
	for _, c := range cmds {
		o, code := b.one(c)
		out.WriteString(o)
		if code != 0 {
			return out.String(), code
		}
	}
	return out.String(), 0
}

func (b *envBox) one(c []string) (string, int) {
	if len(c) == 0 {
		return "", 0
	}
	switch c[0] {
	case "has-session":
		if b.created {
			return "", 0
		}
		return "", 1
	case "new-session":
		b.created = true
		for i := 0; i+1 < len(c); i++ {
			if c[i] == "-e" {
				k, v, _ := strings.Cut(c[i+1], "=")
				b.session[k] = v
			}
		}
		return "%1\n", 0
	case "show-environment":
		if b.showFails {
			return "", 1
		}
		var out strings.Builder
		for _, k := range slices.Sorted(maps.Keys(b.session)) {
			out.WriteString(k + "=" + b.session[k] + "\n")
		}
		for _, k := range slices.Sorted(maps.Keys(b.removed)) {
			out.WriteString("-" + k + "\n")
		}
		return out.String(), 0
	case "display-message":
		return strings.ReplaceAll(c[len(c)-1], "#{"+runtime.TmuxLaunchEnvKeysOption+"}", b.record) + "\n", 0
	case "set-option":
		if len(c) >= 2 && c[len(c)-2] == runtime.TmuxLaunchEnvKeysOption {
			b.record = c[len(c)-1]
		}
		return "", 0
	case "set-environment":
		rest := c[1:]
		mode := ""
		for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
			switch rest[0] {
			case "-t":
				rest = rest[1:]
			case "-r", "-u":
				mode = rest[0]
			}
			rest = rest[1:]
		}
		k := rest[0]
		switch mode {
		case "-r":
			delete(b.session, k)
			b.removed[k] = true
		case "-u":
			delete(b.session, k)
			delete(b.removed, k)
		default:
			b.session[k] = rest[1]
			delete(b.removed, k)
		}
		return "", 0
	case "source-file":
		return b.tmux(shellquote.Split(b.staged), true)
	case "respawn-pane":
		b.respawned = true
		b.respawnEnv = maps.Clone(b.global)
		maps.Copy(b.respawnEnv, b.session)
		for k := range b.removed {
			delete(b.respawnEnv, k)
		}
		return "", 0
	}
	return "", 0
}

// envWriteCalls returns the tmux calls that write the session environment or
// the launch-key record, as issued (argv or a sourced file).
func (b *envBox) envWriteCalls() [][]string {
	var out [][]string
	for _, c := range b.f.calls {
		if len(c) > 0 && c[0] == "tmux" && (slices.Contains(c, "set-environment") || slices.Contains(c, runtime.TmuxLaunchEnvKeysOption) || slices.Contains(c, "source-file")) {
			out = append(out, c)
		}
	}
	return out
}

func relaunchCfg(env map[string]string) runtime.Config {
	return runtime.Config{Command: "agent --resumed", WorkDir: "/w", Env: env}
}

// A launch-only env change is applied by a relaunch in the warm box; the
// relaunched agent must start on the new env, not the one the box was created
// with: changed and added keys carry their new values, a dropped key falls
// back to the server's global value as it would in a fresh session, and the
// incarnation and gc's own session metadata are left alone.
func TestProvider_RelaunchDeliversLaunchEnvChanges(t *testing.T) {
	box := newEnvBox(map[string]string{"PATH": "/bin", "SHARED": "global"})
	p := providerWith(box.f)
	ctx := context.Background()

	start := map[string]string{
		"GC_AGENT":          "a",
		"CHANGED":           "v1",
		"SHARED":            "launch",
		"DROPPED":           "x",
		"EMPTIED":           "set",
		"GC_INSTANCE_TOKEN": "tok-1",
	}
	if err := p.Start(ctx, "s", runtime.Config{Command: "agent", Env: start}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if want := "CHANGED DROPPED EMPTIED GC_AGENT GC_INSTANCE_TOKEN SHARED"; box.record != want {
		t.Errorf("record after Start = %q, want %q", box.record, want)
	}
	box.session["GC_DRAIN"] = "meta" // SetMeta: not a launch key

	next := map[string]string{
		"GC_AGENT":          "a",
		"CHANGED":           "v2",
		"ADDED":             "new",
		"EMPTIED":           "",
		"GC_INSTANCE_TOKEN": "tok-2",
	}
	if err := p.Relaunch(ctx, "s", relaunchCfg(next)); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	if !box.respawned {
		t.Fatal("Relaunch did not respawn the agent")
	}
	got := box.respawnEnv
	for k, want := range map[string]string{
		"CHANGED":           "v2",
		"ADDED":             "new",
		"EMPTIED":           "",
		"GC_AGENT":          "a",
		"SHARED":            "global",
		"PATH":              "/bin",
		"GC_DRAIN":          "meta",
		"GC_INSTANCE_TOKEN": "tok-1",
	} {
		if v, ok := got[k]; !ok || v != want {
			t.Errorf("respawned agent %s = %q (set %v), want %q", k, v, ok, want)
		}
	}
	if v, ok := got["DROPPED"]; ok {
		t.Errorf("respawned agent DROPPED = %q, want unset", v)
	}
	if want := "ADDED CHANGED EMPTIED GC_AGENT GC_INSTANCE_TOKEN"; box.record != want {
		t.Errorf("record after Relaunch = %q, want %q", box.record, want)
	}

	// A later relaunch knows what this one set.
	box.respawned = false
	if err := p.Relaunch(ctx, "s", relaunchCfg(map[string]string{"GC_AGENT": "a", "CHANGED": "v2", "GC_INSTANCE_TOKEN": "tok-1"})); err != nil {
		t.Fatalf("second Relaunch: %v", err)
	}
	if v, ok := box.respawnEnv["ADDED"]; ok {
		t.Errorf("after the second relaunch ADDED = %q, want unset", v)
	}
	if box.respawnEnv["GC_DRAIN"] != "meta" {
		t.Errorf("second relaunch dropped session metadata GC_DRAIN")
	}
}

// A changed credential reaches the relaunched agent, and neither end's command
// line ever carries it: the env update rides the staged tmux command file.
func TestProvider_RelaunchDeliversChangedSecretOutsideArgv(t *testing.T) {
	const oldSecret = "sk-test-old-not-a-real-credential"
	const newSecret = "sk-test-new-not-a-real-credential"
	box := newEnvBox(nil)
	p := providerWith(box.f)
	ctx := context.Background()
	if err := p.Start(ctx, "s", runtime.Config{Command: "agent", Env: map[string]string{"OPENAI_API_KEY": oldSecret, "GC_RIG": "r"}}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Relaunch(ctx, "s", relaunchCfg(map[string]string{"OPENAI_API_KEY": newSecret, "GC_RIG": "r"})); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	for _, c := range box.f.calls {
		for _, a := range c {
			if strings.Contains(a, newSecret) || strings.Contains(a, oldSecret) {
				t.Fatalf("secret value reached remote argv: %v", c)
			}
		}
	}
	if got := box.respawnEnv["OPENAI_API_KEY"]; got != newSecret {
		t.Errorf("respawned agent OPENAI_API_KEY = %q, want the new value", got)
	}
	if firstCall(box.f, func(argv []string) bool {
		return slices.Equal(argv, []string{"tmux", "source-file", stagedDir + "/" + tmuxFileName})
	}) == nil {
		t.Error("the env update was not sourced from the staged tmux command file")
	}
}

// A value tmux would read as a command separator when passed as an argument
// arrives intact. Both keys are inert, so the update rides argv.
func TestProvider_RelaunchDeliversValueEndingInSemicolon(t *testing.T) {
	box := newEnvBox(nil)
	box.warm(map[string]string{"GC_RIG": "r"})
	if err := providerWith(box.f).Relaunch(context.Background(), "s", relaunchCfg(map[string]string{"GC_RIG": "r", "GC_ALIAS": "a;b;"})); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	if firstCall(box.f, isStagingScript) != nil {
		t.Fatal("an all-inert env must not stage a file; this test covers the argv path")
	}
	if got := box.respawnEnv["GC_ALIAS"]; got != "a;b;" {
		t.Errorf("respawned agent GC_ALIAS = %q, want %q", got, "a;b;")
	}
}

// An unchanged env costs the one read and writes nothing.
func TestProvider_RelaunchUnchangedEnvWritesNothing(t *testing.T) {
	box := newEnvBox(nil)
	p := providerWith(box.f)
	ctx := context.Background()
	env := map[string]string{"GC_AGENT": "a", "GC_RIG": "r", "GC_INSTANCE_TOKEN": "tok-1"}
	if err := p.Start(ctx, "s", runtime.Config{Command: "agent", Env: env}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	before := len(box.envWriteCalls())
	next := maps.Clone(env)
	next["GC_INSTANCE_TOKEN"] = "tok-2" // the incarnation keys are never rewritten
	if err := p.Relaunch(ctx, "s", relaunchCfg(next)); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	if writes := box.envWriteCalls()[before:]; len(writes) != 0 {
		t.Errorf("unchanged relaunch wrote %v, want no env or record writes", writes)
	}
	if !box.respawned {
		t.Error("Relaunch did not respawn the agent")
	}
}

// A box created before the record existed still gets changed and added
// values, but nothing is removed from it: its session env cannot tell a
// launch key from metadata. The relaunch writes the record, so the next one can.
func TestProvider_RelaunchOnUnrecordedBoxRemovesNothing(t *testing.T) {
	box := newEnvBox(nil)
	box.warm(map[string]string{"GC_AGENT": "a", "OLD": "x"})
	if err := providerWith(box.f).Relaunch(context.Background(), "s", relaunchCfg(map[string]string{"GC_AGENT": "b"})); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	if box.respawnEnv["GC_AGENT"] != "b" {
		t.Errorf("respawned agent GC_AGENT = %q, want b", box.respawnEnv["GC_AGENT"])
	}
	if box.respawnEnv["OLD"] != "x" {
		t.Errorf("unrecorded box lost OLD; a relaunch with no record must remove nothing")
	}
	if box.record != "GC_AGENT" {
		t.Errorf("record = %q, want GC_AGENT", box.record)
	}
}

// A box whose environment cannot be read is not respawned: the caller falls
// back to a full restart, which applies the env.
func TestProvider_RelaunchEnvReadFailureFailsBeforeRespawn(t *testing.T) {
	box := newEnvBox(nil)
	box.warm(map[string]string{"GC_AGENT": "a"})
	box.showFails = true
	if err := providerWith(box.f).Relaunch(context.Background(), "s", relaunchCfg(map[string]string{"GC_AGENT": "b"})); err == nil {
		t.Fatal("Relaunch must fail when the session env cannot be read")
	}
	if box.respawned {
		t.Error("the agent was respawned on an env that was never brought up to date")
	}
}
