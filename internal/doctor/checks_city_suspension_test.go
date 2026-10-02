package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
)

func writeSuspensionStateFile(t *testing.T, cityPath, body string) {
	t.Helper()
	p := citylayout.SuspensionStateFile(cityPath)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCitySuspensionCheck_OK_NotSuspended(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.City{Workspace: config.Workspace{SuspendedOnStart: false}}
	r := NewCitySuspensionCheck(cfg).Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Fatalf("status = %v, want StatusOK; message: %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "not suspended") {
		t.Errorf("message should confirm the city is not suspended, got: %q", r.Message)
	}
}

func TestCitySuspensionCheck_Warns_ConfiguredDefault(t *testing.T) {
	dir := t.TempDir()
	// No runtime override written; suspension comes solely from the
	// configured suspended_on_start default.
	cfg := &config.City{Workspace: config.Workspace{SuspendedOnStart: true}}
	r := NewCitySuspensionCheck(cfg).Run(&CheckContext{CityPath: dir})
	if r.Status != StatusWarning {
		t.Fatalf("status = %v, want StatusWarning; message: %s", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Errorf("severity = %v, want SeverityAdvisory", r.Severity)
	}
	if !strings.Contains(r.Message, "suspended_on_start") || !strings.Contains(r.Message, "configured") {
		t.Errorf("message should distinguish the configured default, got: %q", r.Message)
	}
	if strings.Contains(r.Message, "explicit runtime override") {
		t.Errorf("message should not claim an explicit override when none exists, got: %q", r.Message)
	}
}

func TestCitySuspensionCheck_Warns_ExplicitOverride(t *testing.T) {
	dir := t.TempDir()
	writeSuspensionStateFile(t, dir, `{"city":{"suspended":true}}`)
	// Configured default is false; the explicit runtime override alone
	// drives the effective state to suspended.
	cfg := &config.City{Workspace: config.Workspace{SuspendedOnStart: false}}
	r := NewCitySuspensionCheck(cfg).Run(&CheckContext{CityPath: dir})
	if r.Status != StatusWarning {
		t.Fatalf("status = %v, want StatusWarning; message: %s", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Errorf("severity = %v, want SeverityAdvisory", r.Severity)
	}
	if !strings.Contains(r.Message, "explicit runtime override") || !strings.Contains(r.Message, "gc suspend") {
		t.Errorf("message should distinguish the explicit runtime override, got: %q", r.Message)
	}
}

func TestCitySuspensionCheck_ExplicitResumeOverridesConfiguredDefault(t *testing.T) {
	dir := t.TempDir()
	writeSuspensionStateFile(t, dir, `{"city":{"suspended":false}}`)
	// Configured default says suspended, but an explicit resume override
	// wins per suspensionstate.EffectiveCitySuspended.
	cfg := &config.City{Workspace: config.Workspace{SuspendedOnStart: true}}
	r := NewCitySuspensionCheck(cfg).Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Fatalf("status = %v, want StatusOK (explicit resume should win); message: %s", r.Status, r.Message)
	}
}

func TestCitySuspensionCheck_UnreadableState_ReportsUnknown(t *testing.T) {
	dir := t.TempDir()
	// Corrupt JSON: suspensionstate.Load returns an error (distinct from
	// the missing-file case, which returns a zero-value State and no
	// error).
	writeSuspensionStateFile(t, dir, `{not valid json`)
	cfg := &config.City{Workspace: config.Workspace{SuspendedOnStart: false}}
	r := NewCitySuspensionCheck(cfg).Run(&CheckContext{CityPath: dir})
	if r.Status != StatusWarning {
		t.Fatalf("status = %v, want StatusWarning for an unreadable state file", r.Status)
	}
	if !strings.Contains(strings.ToLower(r.Message), "unknown") {
		t.Errorf("message should say the suspension state is unknown, got: %q", r.Message)
	}
	if strings.Contains(strings.ToLower(r.Message), "not suspended") {
		t.Errorf("an unreadable state must never be inferred as not-suspended, got: %q", r.Message)
	}
}

func TestCitySuspensionCheck_NoConfig(t *testing.T) {
	r := NewCitySuspensionCheck(nil).Run(&CheckContext{CityPath: t.TempDir()})
	if r.Status != StatusOK {
		t.Errorf("nil config should not trigger a warning, got %v", r.Status)
	}
}

func TestCitySuspensionCheck_NilContext(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{SuspendedOnStart: true}}
	r := NewCitySuspensionCheck(cfg).Run(nil)
	if r.Status != StatusOK {
		t.Errorf("nil context should not trigger a warning, got %v", r.Status)
	}
}

func TestCitySuspensionCheck_NotFixable(t *testing.T) {
	c := NewCitySuspensionCheck(&config.City{})
	if c.CanFix() {
		t.Error("city suspension must never be auto-fixed (no auto-resume)")
	}
	if err := c.Fix(&CheckContext{CityPath: t.TempDir()}); err != nil {
		t.Errorf("Fix should be a no-op, got error: %v", err)
	}
}

func TestCitySuspensionCheck_WarmupEligible(t *testing.T) {
	if !NewCitySuspensionCheck(nil).WarmupEligible() {
		t.Error("the check should opt into warmup so suspension surfaces on `gc start`")
	}
}
