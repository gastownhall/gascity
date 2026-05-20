package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestLegacySuspendedFieldCheck_OK_NoLegacyFields(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{SuspendedOnStart: true},
		Rigs: []config.Rig{
			{Name: "alpha", SuspendedOnStart: true},
			{Name: "beta"},
		},
	}
	r := NewLegacySuspendedFieldCheck(cfg).Run(nil)
	if r.Status != StatusOK {
		t.Fatalf("status = %v, want StatusOK; details: %v", r.Status, r.Details)
	}
}

func TestLegacySuspendedFieldCheck_Warns_WorkspaceLegacy(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Suspended: true}}
	r := NewLegacySuspendedFieldCheck(cfg).Run(nil)
	if r.Status != StatusWarning {
		t.Fatalf("status = %v, want StatusWarning", r.Status)
	}
	if len(r.Details) != 1 {
		t.Fatalf("expected 1 detail, got %d: %v", len(r.Details), r.Details)
	}
	if !strings.Contains(r.Details[0], "[workspace] suspended") ||
		!strings.Contains(r.Details[0], "suspended_on_start") {
		t.Errorf("workspace warning should mention [workspace] suspended and the new spelling suspended_on_start, got: %q", r.Details[0])
	}
	if !strings.Contains(strings.ToLower(r.Details[0]), "alias") {
		t.Errorf("warning should call out that the legacy field is honored as an alias, got: %q", r.Details[0])
	}
}

func TestLegacySuspendedFieldCheck_Warns_RigLegacy(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "alpha", Suspended: true},
			{Name: "beta"},
			{Name: "gamma", Suspended: true},
		},
	}
	r := NewLegacySuspendedFieldCheck(cfg).Run(nil)
	if r.Status != StatusWarning {
		t.Fatalf("status = %v, want StatusWarning", r.Status)
	}
	if len(r.Details) != 2 {
		t.Fatalf("expected 2 details, got %d: %v", len(r.Details), r.Details)
	}
	joined := strings.Join(r.Details, "\n")
	if !strings.Contains(joined, `"alpha"`) || !strings.Contains(joined, `"gamma"`) {
		t.Errorf("rig warnings should reference each offending rig by name, got: %s", joined)
	}
	if strings.Contains(joined, `"beta"`) {
		t.Errorf("rig with no legacy field should not appear in warnings, got: %s", joined)
	}
	for _, d := range r.Details {
		if !strings.Contains(d, "suspended_on_start") {
			t.Errorf("rig warning must mention the new spelling suspended_on_start, got: %q", d)
		}
	}
}

func TestLegacySuspendedFieldCheck_Warns_Both(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Suspended: true},
		Rigs:      []config.Rig{{Name: "alpha", Suspended: true}},
	}
	r := NewLegacySuspendedFieldCheck(cfg).Run(nil)
	if r.Status != StatusWarning {
		t.Fatalf("status = %v, want StatusWarning", r.Status)
	}
	if len(r.Details) != 2 {
		t.Errorf("expected 2 details (workspace + 1 rig), got %d: %v", len(r.Details), r.Details)
	}
	if r.FixHint == "" {
		t.Error("FixHint should be set so users know how to migrate")
	}
	if !strings.Contains(r.FixHint, "--fix") {
		t.Error("FixHint should point at --fix")
	}
}

func TestLegacySuspendedFieldCheck_NoConfig(t *testing.T) {
	r := NewLegacySuspendedFieldCheck(nil).Run(nil)
	if r.Status != StatusOK {
		t.Errorf("nil config should not trigger warning, got %v", r.Status)
	}
}

func TestLegacySuspendedFieldCheck_AutoFixable(t *testing.T) {
	c := NewLegacySuspendedFieldCheck(&config.City{Workspace: config.Workspace{Suspended: true}})
	if !c.CanFix() {
		t.Error("the legacy-suspended migration is mechanical (key rename) and should be auto-fixable")
	}
}

func TestLegacySuspendedFieldCheck_WarmupEligible(t *testing.T) {
	if !NewLegacySuspendedFieldCheck(nil).WarmupEligible() {
		t.Error("the check should opt into warmup so the warning surfaces on `gc start`")
	}
}

// --- rewriteLegacySuspended (pure-string form of Fix) ---

func TestRewriteLegacySuspended_RenamesWorkspaceField(t *testing.T) {
	src := `# top comment
[workspace]
name = "demo"
suspended = true  # was set ages ago

[[agent]]
name = "mayor"
`
	out, n := rewriteLegacySuspended(src)
	if n != 1 {
		t.Fatalf("changes = %d, want 1; out:\n%s", n, out)
	}
	if !strings.Contains(out, `suspended_on_start = true  # was set ages ago`) {
		t.Errorf("expected workspace key rename with trailing comment preserved; got:\n%s", out)
	}
	if strings.Contains(out, `suspended = true`) {
		t.Errorf("original `suspended = true` should be gone after rename; got:\n%s", out)
	}
}

func TestRewriteLegacySuspended_RenamesRigField(t *testing.T) {
	src := `[workspace]
name = "demo"

[[rigs]]
name = "alpha"
path = "/tmp/alpha"
suspended = true
`
	out, n := rewriteLegacySuspended(src)
	if n != 1 {
		t.Fatalf("changes = %d, want 1; out:\n%s", n, out)
	}
	if !strings.Contains(out, "suspended_on_start = true") {
		t.Errorf("expected rig key rename; got:\n%s", out)
	}
}

func TestRewriteLegacySuspended_LeavesAgentSectionsAlone(t *testing.T) {
	// Agent-scope migration is tracked in #2407; the doctor fix here
	// must not silently touch agent sections.
	src := `[workspace]
name = "demo"

[[agent]]
name = "mayor"
suspended = true
`
	out, n := rewriteLegacySuspended(src)
	if n != 0 {
		t.Errorf("expected zero changes (agent is out of scope), got %d:\n%s", n, out)
	}
	if !strings.Contains(out, `[[agent]]
name = "mayor"
suspended = true
`) {
		t.Errorf("agent section must be byte-identical; got:\n%s", out)
	}
}

func TestRewriteLegacySuspended_SkipsWhenSuspendedOnStartAlreadyPresent(t *testing.T) {
	// A section that already declares suspended_on_start must not be
	// rewritten — the alias semantics already keep behavior identical
	// at read time, and the user can choose which spelling to keep.
	src := `[workspace]
name = "demo"
suspended = true
suspended_on_start = false
`
	out, n := rewriteLegacySuspended(src)
	if n != 0 {
		t.Errorf("expected zero changes when both keys present, got %d:\n%s", n, out)
	}
	if out != src {
		t.Errorf("document should be byte-identical when fix is skipped; got:\n%s", out)
	}
}

func TestRewriteLegacySuspended_PreservesIndentationAndFalseValue(t *testing.T) {
	src := "[[rigs]]\n\tname = \"alpha\"\n\tsuspended = false\n"
	out, n := rewriteLegacySuspended(src)
	if n != 1 {
		t.Fatalf("changes = %d, want 1; out:\n%s", n, out)
	}
	if !strings.Contains(out, "\tsuspended_on_start = false\n") {
		t.Errorf("expected indentation and false value preserved; got:\n%s", out)
	}
}

func TestRewriteLegacySuspended_NoOpWhenNoLegacyFields(t *testing.T) {
	src := `[workspace]
name = "demo"
suspended_on_start = true
`
	out, n := rewriteLegacySuspended(src)
	if n != 0 {
		t.Errorf("expected zero changes, got %d", n)
	}
	if out != src {
		t.Errorf("document should be byte-identical when no legacy fields; got:\n%s", out)
	}
}

func TestLegacySuspendedFieldCheck_Fix_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "city.toml")
	body := []byte(`[workspace]
name = "demo"
suspended = true

[[rigs]]
name = "alpha"
suspended = true

[[agent]]
name = "mayor"
suspended = true
`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewLegacySuspendedFieldCheck(&config.City{
		Workspace: config.Workspace{Suspended: true},
		Rigs:      []config.Rig{{Name: "alpha", Suspended: true}},
	})
	if err := c.Fix(&CheckContext{CityPath: dir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	rewritten, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(rewritten)
	if !strings.Contains(got, "[workspace]\nname = \"demo\"\nsuspended_on_start = true\n") {
		t.Errorf("workspace not rewritten; got:\n%s", got)
	}
	if !strings.Contains(got, "[[rigs]]\nname = \"alpha\"\nsuspended_on_start = true\n") {
		t.Errorf("rig not rewritten; got:\n%s", got)
	}
	if !strings.Contains(got, "[[agent]]\nname = \"mayor\"\nsuspended = true\n") {
		t.Errorf("agent section must remain untouched (out of scope per #2407); got:\n%s", got)
	}
}

func TestLegacySuspendedFieldCheck_Fix_MissingFileIsNoOp(t *testing.T) {
	dir := t.TempDir()
	c := NewLegacySuspendedFieldCheck(&config.City{})
	if err := c.Fix(&CheckContext{CityPath: dir}); err != nil {
		t.Errorf("Fix on missing city.toml should not error, got: %v", err)
	}
}

func TestLegacySuspendedFieldCheck_Fix_NilContextIsNoOp(t *testing.T) {
	c := NewLegacySuspendedFieldCheck(&config.City{})
	if err := c.Fix(nil); err != nil {
		t.Errorf("Fix with nil ctx should not error, got: %v", err)
	}
}
