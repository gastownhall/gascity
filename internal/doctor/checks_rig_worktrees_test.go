package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// newRigWorktreesCheckWithMeasure builds the check with an injected
// measurement so tests never shell out to du.
func newRigWorktreesCheckWithMeasure(t *testing.T, rigPath string, cfg config.DoctorConfig, measure func(string) (int64, bool, error)) *RigWorktreesCheck {
	t.Helper()
	c := NewRigWorktreesCheck(config.Rig{Name: "testrig", Path: rigPath}, cfg)
	c.measureDir = measure
	return c
}

// fixedSize returns a measurement stub reporting n bytes for an
// existing directory.
func fixedSize(n int64) func(string) (int64, bool, error) {
	return func(string) (int64, bool, error) { return n, true, nil }
}

// makeWorktreeDirs creates <rigPath>/worktrees/<name> for each name and
// returns the rig path.
func makeWorktreeDirs(t *testing.T, names ...string) string {
	t.Helper()
	rigPath := t.TempDir()
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(rigPath, "worktrees", n), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", n, err)
		}
	}
	return rigPath
}

func TestRigWorktreesCheck_Name(t *testing.T) {
	c := NewRigWorktreesCheck(config.Rig{Name: "eunice", Path: "/tmp/x"}, config.DoctorConfig{})
	if got, want := c.Name(), "rig:eunice:worktrees"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
}

// The regression this check exists for: a rig holding a large per-bead
// worktree population must be reported, not reported as absent. Before
// this check, every worktree check was scoped to $CITY/.gc/worktrees and
// the board came back green over tens of gigabytes.
func TestRigWorktreesCheck_PopulatedOverErrorThreshold_ReportsCountAndSize(t *testing.T) {
	rigPath := makeWorktreeDirs(t, "tlp-aaa", "tlp-bbb", "tlp-ccc")
	cfg := config.DoctorConfig{WorktreeRigWarnSize: "10GB", WorktreeRigErrorSize: "50GB"}
	c := newRigWorktreesCheckWithMeasure(t, rigPath, cfg, fixedSize(60*1024*1024*1024))

	r := c.Run(&CheckContext{})

	if r.Status != StatusError {
		t.Fatalf("status = %d (%s), want StatusError", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "3 per-bead worktree(s)") {
		t.Errorf("message = %q, want the worktree count", r.Message)
	}
	if !strings.Contains(r.Message, "60.0 GB") {
		t.Errorf("message = %q, want the aggregate size", r.Message)
	}
	if strings.Contains(r.Message, "no worktrees") {
		t.Errorf("message = %q, must not claim absence over a populated directory", r.Message)
	}
	if r.FixHint == "" {
		t.Error("FixHint empty; want a pointer at the prune order")
	}
}

func TestRigWorktreesCheck_OverWarnUnderError_Warns(t *testing.T) {
	rigPath := makeWorktreeDirs(t, "tlp-aaa")
	cfg := config.DoctorConfig{WorktreeRigWarnSize: "10GB", WorktreeRigErrorSize: "50GB"}
	c := newRigWorktreesCheckWithMeasure(t, rigPath, cfg, fixedSize(20*1024*1024*1024))

	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "warn threshold") {
		t.Errorf("message = %q, want the warn threshold named", r.Message)
	}
}

func TestRigWorktreesCheck_UnderThresholds_OKButStillReportsCount(t *testing.T) {
	rigPath := makeWorktreeDirs(t, "tlp-aaa", "tlp-bbb")
	cfg := config.DoctorConfig{WorktreeRigWarnSize: "10GB", WorktreeRigErrorSize: "50GB"}
	c := newRigWorktreesCheckWithMeasure(t, rigPath, cfg, fixedSize(1024*1024*1024))

	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK", r.Status, r.Message)
	}
	// An OK status must still be honest about what is there — the
	// original bug was a green line that implied nothing existed.
	if !strings.Contains(r.Message, "2 per-bead worktree(s)") {
		t.Errorf("message = %q, want the count even when under threshold", r.Message)
	}
	if r.FixHint != "" {
		t.Errorf("FixHint = %q, want empty for OK result", r.FixHint)
	}
}

func TestRigWorktreesCheck_NoWorktreesDir_OKAndNamesThePath(t *testing.T) {
	rigPath := t.TempDir()
	c := newRigWorktreesCheckWithMeasure(t, rigPath, config.DoctorConfig{}, fixedSize(0))

	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK", r.Status, r.Message)
	}
	// Naming the path is the point: "no worktrees directory" unqualified
	// is what made the old message a false claim.
	if !strings.Contains(r.Message, filepath.Join(rigPath, "worktrees")) {
		t.Errorf("message = %q, want the absent path named", r.Message)
	}
}

func TestRigWorktreesCheck_EmptyWorktreesDir_OK(t *testing.T) {
	rigPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigPath, "worktrees"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	c := newRigWorktreesCheckWithMeasure(t, rigPath, config.DoctorConfig{}, fixedSize(0))

	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "no per-bead worktrees") {
		t.Errorf("message = %q, want empty-directory wording", r.Message)
	}
}

func TestRigWorktreesCheck_FilesAreNotCountedAsWorktrees(t *testing.T) {
	rigPath := makeWorktreeDirs(t, "tlp-aaa")
	if err := os.WriteFile(filepath.Join(rigPath, "worktrees", "README"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	c := newRigWorktreesCheckWithMeasure(t, rigPath, config.DoctorConfig{}, fixedSize(1))

	r := c.Run(&CheckContext{})

	if !strings.Contains(r.Message, "1 per-bead worktree(s)") {
		t.Errorf("message = %q, want only the directory counted", r.Message)
	}
}

// "We can't tell" must not read as "we're fine" — the same policy
// WorktreeDiskSizeCheck applies to measurement failure.
func TestRigWorktreesCheck_MeasureFails_WarnsAndKeepsTheCount(t *testing.T) {
	rigPath := makeWorktreeDirs(t, "tlp-aaa", "tlp-bbb")
	c := newRigWorktreesCheckWithMeasure(t, rigPath, config.DoctorConfig{}, func(string) (int64, bool, error) {
		return 0, false, errors.New("du exploded")
	})

	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "2 per-bead worktree(s)") {
		t.Errorf("message = %q, want the count preserved when size is unknown", r.Message)
	}
	if len(r.Details) == 0 || !strings.Contains(strings.Join(r.Details, " "), "du exploded") {
		t.Errorf("Details = %v, want the underlying measurement error", r.Details)
	}
}

// Removal belongs to the worktree-prune order, which owns the safety
// criterion. doctor --fix must not grow a second, weaker opinion.
func TestRigWorktreesCheck_IsObservationOnly(t *testing.T) {
	c := NewRigWorktreesCheck(config.Rig{Name: "r", Path: t.TempDir()}, config.DoctorConfig{})
	if c.CanFix() {
		t.Error("CanFix() = true, want false — removal is the prune order's job")
	}
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Errorf("Fix() = %v, want nil no-op", err)
	}
	if c.WarmupEligible() {
		t.Error("WarmupEligible() = true, want false")
	}
}

// The city-scoped check's message was a positive false claim: it said
// "no worktrees directory" on a box whose rig worktrees/ held 28 GB.
func TestWorktreeCheck_AbsentCityDirMessageNamesGcWorktrees(t *testing.T) {
	c := &WorktreeCheck{}

	r := c.Run(&CheckContext{CityPath: t.TempDir()})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, ".gc/worktrees") {
		t.Errorf("message = %q, want the .gc/worktrees scope named", r.Message)
	}
}

func TestWorktreeCheck_EmptyCityDirMessageNamesGcWorktrees(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "worktrees"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	c := &WorktreeCheck{}

	r := c.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, ".gc/worktrees") {
		t.Errorf("message = %q, want the .gc/worktrees scope named", r.Message)
	}
}
