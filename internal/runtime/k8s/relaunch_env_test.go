package k8s

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// podBox models what a respawned agent inherits inside a warm pod: the
// container env from the pod spec (the in-pod tmux server's global
// environment), the "main" session's environment with its -r removal marks,
// the session's launch-key record, and respawn-pane building the child's
// environment from them. A running pod's spec env cannot change, so a
// relaunch can only reach the agent through the session environment.
type podBox struct {
	fake *fakeK8sOps

	podEnv  map[string]string
	session map[string]string
	removed map[string]bool
	record  string

	respawned  bool
	respawnEnv map[string]string
	showFails  bool
}

// newPodBox adds a running pod for session "s" whose agent container was
// created from env, as Start would have built it.
func newPodBox(t *testing.T, p *Provider, fake *fakeK8sOps, env map[string]string) *podBox {
	t.Helper()
	cfg := runtime.Config{Command: "agent", Env: env}
	vars, err := buildPodEnv(env, projectedPodWorkDir(cfg), p.managedServiceHost, p.managedServicePort)
	if err != nil {
		t.Fatalf("buildPodEnv: %v", err)
	}
	fake.pods["s"] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "s",
			Labels: map[string]string{"app": "gc-agent", "gc-session": "s"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "staging"},
			{Name: "agent", Env: vars},
		}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	b := &podBox{fake: fake, podEnv: map[string]string{}, session: map[string]string{}, removed: map[string]bool{}}
	for _, v := range vars {
		if v.ValueFrom == nil {
			b.podEnv[v.Name] = v.Value
		}
	}
	fake.execFunc = b.exec
	return b
}

func (b *podBox) exec(_ string, cmd []string) (string, error) {
	switch {
	case len(cmd) == 1 && cmd[0] == "sh":
		args := shellquote.Split(b.fake.calls[len(b.fake.calls)-1].stdin)
		if len(args) > 0 && args[0] == "exec" {
			args = args[1:]
		}
		if len(args) == 0 || args[0] != "tmux" {
			return "", errors.New("unexpected script")
		}
		return b.tmux(args[1:])
	case len(cmd) >= 3 && cmd[0] == "sh" && cmd[1] == "-c" && strings.Contains(cmd[2], "respawn-pane"):
		b.respawned = true
		b.respawnEnv = maps.Clone(b.podEnv)
		maps.Copy(b.respawnEnv, b.session)
		for k := range b.removed {
			delete(b.respawnEnv, k)
		}
		return "", nil
	case len(cmd) > 0 && cmd[0] == "tmux":
		return b.tmux(cmd[1:])
	}
	return "", nil
}

func (b *podBox) tmux(args []string) (string, error) {
	var cmds [][]string
	var cur []string
	for _, a := range args {
		switch {
		case a == ";":
			cmds, cur = append(cmds, cur), nil
		case strings.HasSuffix(a, `\;`):
			cur = append(cur, a[:len(a)-2]+";")
		default:
			cur = append(cur, a)
		}
	}
	cmds = append(cmds, cur)
	var out strings.Builder
	for _, c := range cmds {
		switch c[0] {
		case "show-environment":
			if b.showFails {
				return "", errors.New("command terminated with exit code 1")
			}
			for _, k := range slices.Sorted(maps.Keys(b.session)) {
				out.WriteString(k + "=" + b.session[k] + "\n")
			}
			for _, k := range slices.Sorted(maps.Keys(b.removed)) {
				out.WriteString("-" + k + "\n")
			}
		case "display-message":
			out.WriteString(strings.ReplaceAll(c[len(c)-1], "#{"+runtime.TmuxLaunchEnvKeysOption+"}", b.record) + "\n")
		case "set-option":
			if c[len(c)-2] == runtime.TmuxLaunchEnvKeysOption {
				b.record = c[len(c)-1]
			}
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
		}
	}
	return out.String(), nil
}

func (b *podBox) envScripts() []string {
	var out []string
	for _, c := range b.fake.calls {
		if c.method == "execInPod" && len(c.cmd) == 1 && c.cmd[0] == "sh" {
			out = append(out, c.stdin)
		}
	}
	return out
}

// A launch-only env change is applied by a relaunch in the warm pod; the
// relaunched agent must start on the new env, not the pod spec's: changed and
// added keys carry their new (pod-projected) values, a key the new env drops is
// hidden even though the container env still sets it, and the incarnation and
// gc's own session metadata are left alone.
func TestProvider_RelaunchDeliversLaunchEnvChanges(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	start := map[string]string{
		"GC_CITY":           "/city",
		"GC_AGENT":          "a",
		"CHANGED":           "v1",
		"DROPPED":           "x",
		"GC_INSTANCE_TOKEN": "tok-1",
	}
	box := newPodBox(t, p, fake, start)
	box.session["GC_DRAIN"] = "meta" // SetMeta: not a launch key

	next := map[string]string{
		"GC_CITY":           "/city",
		"GC_AGENT":          "a",
		"CHANGED":           "v2",
		"ADDED":             "new",
		"GC_INSTANCE_TOKEN": "tok-2",
	}
	if err := p.Relaunch(context.Background(), "s", runtime.Config{Command: "agent", Env: next}); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	if !box.respawned {
		t.Fatal("Relaunch did not respawn the agent")
	}
	for k, want := range map[string]string{
		"CHANGED":           "v2",
		"ADDED":             "new",
		"GC_AGENT":          "a",
		"GC_CITY":           "/workspace", // projected, as the pod spec has it
		"GC_DRAIN":          "meta",
		"GC_INSTANCE_TOKEN": "tok-1",
	} {
		if v, ok := box.respawnEnv[k]; !ok || v != want {
			t.Errorf("respawned agent %s = %q (set %v), want %q", k, v, ok, want)
		}
	}
	if v, ok := box.respawnEnv["DROPPED"]; ok {
		t.Errorf("respawned agent DROPPED = %q, want it hidden", v)
	}

	// A later relaunch re-adds the dropped key and drops the one the first
	// relaunch added.
	box.respawned = false
	again := map[string]string{"GC_CITY": "/city", "GC_AGENT": "a", "CHANGED": "v2", "DROPPED": "back", "GC_INSTANCE_TOKEN": "tok-1"}
	if err := p.Relaunch(context.Background(), "s", runtime.Config{Command: "agent", Env: again}); err != nil {
		t.Fatalf("second Relaunch: %v", err)
	}
	if box.respawnEnv["DROPPED"] != "back" {
		t.Errorf("after the second relaunch DROPPED = %q, want back", box.respawnEnv["DROPPED"])
	}
	if v, ok := box.respawnEnv["ADDED"]; ok {
		t.Errorf("after the second relaunch ADDED = %q, want unset", v)
	}
	if box.respawnEnv["GC_DRAIN"] != "meta" {
		t.Error("second relaunch dropped session metadata GC_DRAIN")
	}
}

// The env update reaches the pod on the exec's stdin, never in the exec
// command, which the API server receives (and may audit-log) as the request.
func TestProvider_RelaunchKeepsEnvValuesOutOfExecArgv(t *testing.T) {
	const secret = "sk-test-not-a-real-credential"
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	box := newPodBox(t, p, fake, map[string]string{"GC_AGENT": "a", "OPENAI_API_KEY": "sk-test-old"})
	if err := p.Relaunch(context.Background(), "s", runtime.Config{Command: "agent", Env: map[string]string{"GC_AGENT": "a", "OPENAI_API_KEY": secret}}); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	for _, c := range fake.calls {
		for _, a := range c.cmd {
			if strings.Contains(a, secret) {
				t.Fatalf("env value reached exec argv: %v", c.cmd)
			}
		}
	}
	if box.respawnEnv["OPENAI_API_KEY"] != secret {
		t.Errorf("respawned agent OPENAI_API_KEY = %q, want the new value", box.respawnEnv["OPENAI_API_KEY"])
	}
}

// An unchanged env costs the one read and writes nothing; pod-projected keys
// (GC_CITY and the like) compare by their projected values.
func TestProvider_RelaunchUnchangedEnvWritesNothing(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	env := map[string]string{"GC_CITY": "/city", "GC_DIR": "/city/rig", "GC_AGENT": "a", "GC_INSTANCE_TOKEN": "tok-1"}
	box := newPodBox(t, p, fake, env)
	next := maps.Clone(env)
	next["GC_INSTANCE_TOKEN"] = "tok-2" // the incarnation keys are never rewritten
	// The first relaunch only writes the record a pod is created without.
	if err := p.Relaunch(context.Background(), "s", runtime.Config{Command: "agent", Env: next}); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	for _, s := range box.envScripts() {
		if strings.Contains(s, "set-environment") {
			t.Errorf("unchanged relaunch wrote the session env: %s", s)
		}
	}
	before := len(box.envScripts())
	if err := p.Relaunch(context.Background(), "s", runtime.Config{Command: "agent", Env: next}); err != nil {
		t.Fatalf("second Relaunch: %v", err)
	}
	if got := box.envScripts()[before:]; len(got) != 0 {
		t.Errorf("unchanged relaunch wrote %q, want nothing", got)
	}
	if !box.respawned {
		t.Error("Relaunch did not respawn the agent")
	}
}

// A pod whose session environment cannot be read is not respawned: the caller
// falls back to a full restart, which recreates the pod with the new env.
func TestProvider_RelaunchEnvReadFailureFailsBeforeRespawn(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	box := newPodBox(t, p, fake, map[string]string{"GC_AGENT": "a"})
	box.showFails = true
	if err := p.Relaunch(context.Background(), "s", runtime.Config{Command: "agent", Env: map[string]string{"GC_AGENT": "b"}}); err == nil {
		t.Fatal("Relaunch must fail when the session env cannot be read")
	}
	if box.respawned {
		t.Error("the agent was respawned on an env that was never brought up to date")
	}
}
