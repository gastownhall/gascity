package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// stubHooklessCommand replaces the child-command seam for one test and returns
// the recorded argv calls. byArg0 maps the first argument ("prime" or "hook")
// to the stdout the stub returns; a nil entry fails that call.
func stubHooklessCommand(t *testing.T, byArg0 map[string]*string) *[]hooklessCall {
	t.Helper()
	calls := &[]hooklessCall{}
	orig := hooklessCommand
	hooklessCommand = func(_ context.Context, bin string, args []string, env []string, dir string) ([]byte, error) {
		*calls = append(*calls, hooklessCall{bin: bin, args: args, env: env, dir: dir})
		out, ok := byArg0[args[0]]
		if !ok || out == nil {
			return nil, errors.New("boom")
		}
		return []byte(*out), nil
	}
	t.Cleanup(func() { hooklessCommand = orig })
	return calls
}

type hooklessCall struct {
	bin  string
	args []string
	env  []string
	dir  string
}

func (c hooklessCall) envValue(key string) string {
	for _, entry := range c.env {
		if strings.HasPrefix(entry, key+"=") {
			return strings.TrimPrefix(entry, key+"=")
		}
	}
	return ""
}

func primeEnvelope(t *testing.T, content string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return string(raw)
}

func TestHooklessStartupContextComposesPrimeAndHookInject(t *testing.T) {
	prime := primeEnvelope(t, "prime context")
	hook := "hook context"
	calls := stubHooklessCommand(t, map[string]*string{"prime": &prime, "hook": &hook})

	info := sessionpkg.Info{ID: "s-1", SessionName: "fx-tui", Alias: "fx-tui", AgentName: "fx-tui"}
	got := hooklessStartupContext(map[string]string{"GC_BIN": "/opt/gc"}, info, "/city", "/work")

	if want := "prime context\n\nhook context"; got != want {
		t.Fatalf("context = %q, want %q", got, want)
	}
	if len(*calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(*calls))
	}
	if got := strings.Join((*calls)[0].args, " "); got != "prime --hook --json" {
		t.Errorf("prime argv = %q", got)
	}
	if got := strings.Join((*calls)[1].args, " "); got != "hook --inject" {
		t.Errorf("hook argv = %q", got)
	}
	if (*calls)[0].bin != "/opt/gc" {
		t.Errorf("bin = %q, want the launch env's GC_BIN", (*calls)[0].bin)
	}
	if (*calls)[0].dir != "/work" {
		t.Errorf("dir = %q, want the session work dir", (*calls)[0].dir)
	}
}

func TestHooklessHookEnvCarriesSessionIdentityAndHookMarkers(t *testing.T) {
	calls := stubHooklessCommand(t, map[string]*string{})
	info := sessionpkg.Info{ID: "s-1", SessionName: "fx-tui", Alias: "alias-1", AgentName: "agent-1"}
	_ = hooklessStartupContext(map[string]string{"BEADS_DIR": "/beads", "GC_BIN": "/opt/gc"}, info, "/city", "")

	if len(*calls) == 0 {
		t.Fatal("no child command recorded")
	}
	call := (*calls)[0]
	for key, want := range map[string]string{
		"GC_MANAGED_SESSION_HOOK":     "1",
		"GC_HOOK_EVENT_NAME":          "SessionStart",
		"GC_STARTUP_PROMPT_DELIVERED": "1",
		"GC_SESSION_ID":               "s-1",
		"GC_SESSION_NAME":             "fx-tui",
		"GC_ALIAS":                    "alias-1",
		"GC_AGENT":                    "agent-1",
		"GC_CITY_PATH":                "/city",
		"BEADS_DIR":                   "/beads",
	} {
		if got := call.envValue(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	// The child must run in the launch env, not the controller's ambient one.
	if got := call.envValue("GC_CITY_PATH"); got == "/some/other/city" {
		t.Errorf("GC_CITY_PATH came from the ambient env: %q", got)
	}
}

func TestHooklessStartupContextDoesNotRunTestBinary(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	calls := stubHooklessCommand(t, map[string]*string{})
	t.Logf("launch GC_BIN = %q", bin)
	_ = hooklessStartupContext(map[string]string{"GC_BIN": bin}, sessionpkg.Info{ID: "s-1"}, "", "")
	if len(*calls) != 0 {
		t.Fatalf("hookless attempted to execute %q with args %q", (*calls)[0].bin, (*calls)[0].args)
	}
}

func TestHooklessStartupContextRequiresNamedBinary(t *testing.T) {
	calls := stubHooklessCommand(t, map[string]*string{})

	if got := hooklessStartupContext(map[string]string{"PATH": "/usr/bin"}, sessionpkg.Info{ID: "s-1"}, "", ""); got != "" {
		t.Fatalf("context = %q, want empty without a launch GC_BIN", got)
	}
	if len(*calls) != 0 {
		t.Fatalf("child commands = %d, want none without a launch GC_BIN", len(*calls))
	}
}

func TestHooklessStartupContextPrimeFailureIsNotFatal(t *testing.T) {
	stubHooklessCommand(t, map[string]*string{})

	got := hooklessStartupContext(map[string]string{"GC_BIN": "/opt/gc"}, sessionpkg.Info{ID: "s-1"}, "", "")
	if got != "" {
		t.Fatalf("context = %q, want empty on prime failure", got)
	}
}

func TestHooklessStartupContextHookInjectFailureKeepsPrime(t *testing.T) {
	prime := primeEnvelope(t, "prime context")
	stubHooklessCommand(t, map[string]*string{"prime": &prime})

	if got, want := hooklessStartupContext(map[string]string{"GC_BIN": "/opt/gc"}, sessionpkg.Info{ID: "s-1"}, "", ""), "prime context"; got != want {
		t.Fatalf("context = %q, want %q", got, want)
	}
}

func TestHooklessStartupContextSkipsOversizedContext(t *testing.T) {
	prime := primeEnvelope(t, strings.Repeat("x", hooklessContextLimit+1))
	stubHooklessCommand(t, map[string]*string{"prime": &prime})

	if got := hooklessStartupContext(map[string]string{"GC_BIN": "/opt/gc"}, sessionpkg.Info{ID: "s-1"}, "", ""); got != "" {
		t.Fatalf("context = %q, want empty over the limit", got)
	}
}

func TestApplyHooklessStartupContextAppendsToArgvPrompt(t *testing.T) {
	cfg := &runtime.Config{
		PromptSuffix: shellquote.Quote("role prompt"),
		Nudge:        "wake",
	}
	if !applyHooklessStartupContext(cfg, "hook context") {
		t.Fatal("context found no carrier")
	}
	parts := shellquote.Split(cfg.PromptSuffix)
	if len(parts) != 1 || parts[0] != "role prompt\n\nhook context" {
		t.Fatalf("PromptSuffix = %q, want one element holding the prompt and context", cfg.PromptSuffix)
	}
	if cfg.Nudge != "wake" {
		t.Errorf("Nudge = %q, want the argv carrier preferred and the nudge untouched", cfg.Nudge)
	}
}

func TestApplyHooklessStartupContextAppendsToNudge(t *testing.T) {
	cfg := &runtime.Config{Nudge: "role prompt"}
	if !applyHooklessStartupContext(cfg, "hook context") {
		t.Fatal("context found no carrier")
	}
	if want := "role prompt\n\nhook context"; cfg.Nudge != want {
		t.Fatalf("Nudge = %q, want %q", cfg.Nudge, want)
	}
	if cfg.PromptSuffix != "" {
		t.Errorf("PromptSuffix = %q, want empty", cfg.PromptSuffix)
	}
}

func TestApplyHooklessStartupContextWithoutCarrierDropsContext(t *testing.T) {
	cfg := &runtime.Config{}
	if applyHooklessStartupContext(cfg, "hook context") {
		t.Fatal("context reported a carrier for a suppressed prompt")
	}
	if cfg.PromptSuffix != "" || cfg.Nudge != "" {
		t.Fatalf("config changed: suffix %q nudge %q", cfg.PromptSuffix, cfg.Nudge)
	}
}

// TestHooklessInjectionLeavesFingerprintsUnchanged pins the safety property the
// injection relies on: PromptSuffix and Nudge are excluded from every
// fingerprint, so mail changing between launches cannot drive a drift restart.
func TestHooklessInjectionLeavesFingerprintsUnchanged(t *testing.T) {
	cfg := runtime.Config{Command: "fx", Lifecycle: runtime.LifecycleOneShot, PromptSuffix: shellquote.Quote("role prompt"), Nudge: "wake"}
	core, launch, live := runtime.CoreFingerprint(cfg), runtime.LaunchFingerprint(cfg), runtime.LiveFingerprint(cfg)

	applyHooklessStartupContext(&cfg, "hook context")

	if got := runtime.CoreFingerprint(cfg); got != core {
		t.Errorf("CoreFingerprint changed: %s -> %s", core, got)
	}
	if got := runtime.LaunchFingerprint(cfg); got != launch {
		t.Errorf("LaunchFingerprint changed: %s -> %s", launch, got)
	}
	if got := runtime.LiveFingerprint(cfg); got != live {
		t.Errorf("LiveFingerprint changed: %s -> %s", live, got)
	}
}

func TestHooklessStartupContextAppliesRequiresAHooklessProvider(t *testing.T) {
	carrier := func() *runtime.Config { return &runtime.Config{PromptSuffix: "'prompt'"} }
	for _, tc := range []struct {
		name string
		tp   TemplateParams
		cfg  *runtime.Config
		want bool
	}{
		{"hookless provider", TemplateParams{ResolvedProvider: &config.ResolvedProvider{Name: "fx"}}, carrier(), true},
		{"hook-capable provider", TemplateParams{ResolvedProvider: &config.ResolvedProvider{Name: "pi", SupportsHooks: true}}, carrier(), false},
		{"agent hooks installed", TemplateParams{HookEnabled: true, ResolvedProvider: &config.ResolvedProvider{Name: "fx"}}, carrier(), false},
		{"no provider resolved", TemplateParams{}, carrier(), false},
		{"no prompt carrier", TemplateParams{ResolvedProvider: &config.ResolvedProvider{Name: "fx"}}, &runtime.Config{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hooklessStartupContextApplies(tc.tp, tc.cfg); got != tc.want {
				t.Fatalf("applies = %t, want %t", got, tc.want)
			}
		})
	}
}
