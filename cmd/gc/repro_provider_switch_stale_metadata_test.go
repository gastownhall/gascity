package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// TestReproProviderSwitch_ClearsStaleProjectionFields is the regression test
// for gastownhall/gascity#8539. A named session configured on a wrapped
// provider (codex, whose resolution legitimately produces non-empty
// provider_kind/builtin_ancestor/resume_command) switches to a builtin-named
// provider (claude, where BuiltinAncestor == Name so those same fields
// legitimately resolve to empty). Before the fix, queueChangedResolvedProviderSessionMetadata
// only queued provider_kind/builtin_ancestor/resume_command when the newly
// resolved value was non-empty, so the OLD codex values survived the switch
// untouched. The fix must clear (overwrite with empty) these fields whenever
// the stored `provider` differs from the freshly resolved one — i.e.
// resolution clearly succeeded and named a different provider.
func TestReproProviderSwitch_ClearsStaleProjectionFields(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	_ = sp.Start(context.TODO(), "mayor", runtime.Config{Command: "codex --model gpt-5"})

	// Tick 1: session created on the wrapped codex provider. provider_kind,
	// builtin_ancestor, and resume_command are all non-empty.
	codexCmd := "codex --model gpt-5"
	ds := map[string]TemplateParams{
		"mayor": {
			TemplateName: "mayor",
			Command:      codexCmd,
			ResolvedProvider: &config.ResolvedProvider{
				Name: "codex", BuiltinAncestor: "codex-base",
				ResumeFlag: "--resume", ResumeStyle: "flag", ResumeCommand: "codex --resume",
				SessionIDFlag: "--session-id",
			},
		},
	}
	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	beads1 := allSessionBeads(t, store)
	if len(beads1) != 1 {
		t.Fatalf("expected 1 bead after tick 1, got %d", len(beads1))
	}
	before := beads1[0].Metadata
	for _, c := range []struct{ key, want string }{
		{"provider", "codex"},
		{"provider_kind", "codex-base"},
		{"builtin_ancestor", "codex-base"},
		{"resume_command", "codex --resume"},
		{"session_id_flag", "--session-id"},
	} {
		if before[c.key] != c.want {
			t.Fatalf("tick 1: %s = %q, want %q", c.key, before[c.key], c.want)
		}
	}

	// Tick 2: agent.toml switches the provider to claude, a builtin with no
	// wrapped ancestor (BuiltinAncestor == Name), so the freshly resolved
	// provider_kind/builtin_ancestor/resume_command/session_id_flag are all
	// legitimately empty.
	claudeCmd := "claude --dangerously-skip-permissions"
	ds["mayor"] = TemplateParams{
		TemplateName: "mayor",
		Command:      claudeCmd,
		ResolvedProvider: &config.ResolvedProvider{
			Name: "claude", BuiltinAncestor: "claude",
			ResumeFlag: "", ResumeStyle: "", ResumeCommand: "",
			SessionIDFlag: "",
		},
	}
	clk.Advance(time.Minute)
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	got := allSessionBeads(t, store)[0].Metadata
	for _, c := range []struct{ key, want string }{
		{"provider", "claude"},
		{"provider_kind", ""},
		{"builtin_ancestor", ""},
		{"resume_command", ""},
		{"session_id_flag", ""},
	} {
		if got[c.key] != c.want {
			t.Errorf("tick 2: %s = %q, want %q\ngc-8539: a provider switch to a builtin-named provider must clear stale projection fields from the old provider, not leave them frozen", c.key, got[c.key], c.want)
		}
	}

	if stderr.Len() > 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}

// TestReproProviderSwitch_NoClobberWhenProviderUnchanged pins the ORIGINAL
// no-clobber guard that gc-8539's fix must not regress: when the provider
// has NOT changed (resolution is a no-op re-resolve, or genuinely failed and
// returned nil), an empty resolved value must never wipe good stored
// metadata. Only an actual provider switch may write an empty projection
// value.
func TestReproProviderSwitch_NoClobberWhenProviderUnchanged(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	_ = sp.Start(context.TODO(), "mayor", runtime.Config{Command: "codex --model gpt-5"})

	codexCmd := "codex --model gpt-5"
	ds := map[string]TemplateParams{
		"mayor": {
			TemplateName: "mayor",
			Command:      codexCmd,
			ResolvedProvider: &config.ResolvedProvider{
				Name: "codex", BuiltinAncestor: "codex-base",
				ResumeFlag: "--resume", ResumeStyle: "flag", ResumeCommand: "codex --resume",
				SessionIDFlag: "--session-id",
			},
		},
	}
	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	// Tick 2: SAME provider ("codex"), but resolution comes back with empty
	// family/resume fields (e.g. a transient lookup failure that still
	// managed to report the provider name). These must NOT clobber the
	// stored values, because the provider did not change.
	ds["mayor"] = TemplateParams{
		TemplateName: "mayor",
		Command:      codexCmd,
		ResolvedProvider: &config.ResolvedProvider{
			Name: "codex", BuiltinAncestor: "",
			ResumeFlag: "", ResumeStyle: "", ResumeCommand: "",
			SessionIDFlag: "",
		},
	}
	clk.Advance(time.Minute)
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	got := allSessionBeads(t, store)[0].Metadata
	for _, c := range []struct{ key, want string }{
		{"provider", "codex"},
		{"provider_kind", "codex-base"},
		{"builtin_ancestor", "codex-base"},
		{"resume_command", "codex --resume"},
		{"session_id_flag", "--session-id"},
	} {
		if got[c.key] != c.want {
			t.Errorf("tick 2 (same provider, empty resolution): %s = %q, want %q\nmust keep stored value when the provider has not switched", c.key, got[c.key], c.want)
		}
	}

	// Tick 3: a nil ResolvedProvider (resolution failed entirely) must also
	// not clobber anything, mirroring #1044's `command != ""` guard.
	ds["mayor"] = TemplateParams{TemplateName: "mayor", Command: codexCmd, ResolvedProvider: nil}
	clk.Advance(time.Minute)
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)
	got = allSessionBeads(t, store)[0].Metadata
	if got["provider"] != "codex" || got["builtin_ancestor"] != "codex-base" || got["resume_command"] != "codex --resume" {
		t.Errorf("tick 3 (nil ResolvedProvider): metadata clobbered: %+v", got)
	}

	if stderr.Len() > 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}

// TestRefreshLaunchKeys_ClearsStaleResumeFieldsOnProviderSwitch exercises
// reconcile_arms_metadata.go's refreshLaunchKeys directly: the reconciler's
// own metadata-refresh path shares the same clobber-guard shape as
// syncSessionBeads, and must apply the same gc-8539 fix.
func TestRefreshLaunchKeys_ClearsStaleResumeFieldsOnProviderSwitch(t *testing.T) {
	stale := rowRefreshKeys{
		"provider": "codex", "provider_kind": "codex-base", "builtin_ancestor": "codex-base",
		"resume_flag": "--resume", "resume_style": "flag", "resume_command": "codex --resume",
		"session_id_flag": "--session-id",
	}
	tp := TemplateParams{
		Command: "claude --dangerously-skip-permissions",
		ResolvedProvider: &config.ResolvedProvider{
			Name: "claude", BuiltinAncestor: "claude",
		},
	}
	m := session.MetadataPatch{}
	refreshLaunchKeys(m, tp, stale)

	want := map[string]string{
		"provider": "claude", "provider_kind": "", "builtin_ancestor": "",
		"resume_flag": "", "resume_style": "", "resume_command": "",
		"session_id_flag": "",
	}
	for k, v := range want {
		if got, ok := m[k]; !ok || got != v {
			t.Errorf("refreshLaunchKeys: m[%q] = %q (ok=%v), want %q", k, got, ok, v)
		}
	}

	// No-clobber pin: same provider, resolution reports empty fields — must
	// not write anything that would clear stored values.
	sameProviderStale := rowRefreshKeys{
		"provider": "codex", "provider_kind": "codex-base", "builtin_ancestor": "codex-base",
		"resume_flag": "--resume", "resume_style": "flag", "resume_command": "codex --resume",
		"session_id_flag": "--session-id",
	}
	tpSame := TemplateParams{
		Command: "codex --model gpt-5",
		ResolvedProvider: &config.ResolvedProvider{
			Name: "codex", BuiltinAncestor: "",
		},
	}
	m2 := session.MetadataPatch{}
	refreshLaunchKeys(m2, tpSame, sameProviderStale)
	for _, k := range []string{"provider", "provider_kind", "builtin_ancestor", "resume_flag", "resume_style", "resume_command", "session_id_flag"} {
		if v, ok := m2[k]; ok {
			t.Errorf("refreshLaunchKeys (same provider): unexpected write m[%q] = %q, want no write (no-clobber guard)", k, v)
		}
	}
}

// TestResolvedWorkerResumeCommand pins worker_handle.go's resume-command
// fallback: it must only fall back to the stored info.ResumeCommand when the
// provider is unchanged. A cross-provider switch (gc-8539) with an empty
// resolved ResumeCommand must resolve to "" rather than replaying a resume
// command built for the OLD provider.
func TestResolvedWorkerResumeCommand(t *testing.T) {
	cases := []struct {
		name     string
		resolved *config.ResolvedProvider
		info     session.Info
		want     string
	}{
		{
			name:     "resolved value wins",
			resolved: &config.ResolvedProvider{Name: "claude", ResumeCommand: "claude --resume"},
			info:     session.Info{Provider: "claude", ResumeCommand: "stale"},
			want:     "claude --resume",
		},
		{
			name:     "same provider empty resolution falls back to stored value",
			resolved: &config.ResolvedProvider{Name: "codex", ResumeCommand: ""},
			info:     session.Info{Provider: "codex", ResumeCommand: "codex --resume"},
			want:     "codex --resume",
		},
		{
			name:     "provider switch with empty resolution does not fall back",
			resolved: &config.ResolvedProvider{Name: "claude", ResumeCommand: ""},
			info:     session.Info{Provider: "codex", ResumeCommand: "codex --resume"},
			want:     "",
		},
		{
			name:     "nil resolved falls back to stored value",
			resolved: nil,
			info:     session.Info{Provider: "codex", ResumeCommand: "codex --resume"},
			want:     "codex --resume",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolvedWorkerResumeCommand(tc.resolved, tc.info); got != tc.want {
				t.Errorf("resolvedWorkerResumeCommand() = %q, want %q", got, tc.want)
			}
		})
	}
}
