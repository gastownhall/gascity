package doctor

import (
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
		t.Errorf("workspace warning should reference both [workspace] suspended and suspended_on_start, got: %q", r.Details[0])
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
			t.Errorf("rig warning must recommend the rename to suspended_on_start, got: %q", d)
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
}

func TestLegacySuspendedFieldCheck_NoConfig(t *testing.T) {
	r := NewLegacySuspendedFieldCheck(nil).Run(nil)
	if r.Status != StatusOK {
		t.Errorf("nil config should not trigger warning, got %v", r.Status)
	}
}

func TestLegacySuspendedFieldCheck_NotAutoFixable(t *testing.T) {
	c := NewLegacySuspendedFieldCheck(&config.City{Workspace: config.Workspace{Suspended: true}})
	if c.CanFix() {
		t.Error("legacy-suspended migration must remain manual — only the user knows the intended semantics")
	}
}

func TestLegacySuspendedFieldCheck_WarmupEligible(t *testing.T) {
	if !NewLegacySuspendedFieldCheck(nil).WarmupEligible() {
		t.Error("the check should opt into warmup so the warning surfaces on `gc start`")
	}
}
