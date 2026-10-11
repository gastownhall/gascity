package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkSinkLink creates a symlink at sink/<name> -> target, creating the
// sink directory. The target is never created — the link dangles.
func mkDanglingLink(t *testing.T, sink, name, target string) {
	t.Helper()
	if err := os.MkdirAll(sink, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(sink, name)); err != nil {
		t.Fatal(err)
	}
}

func TestSkillDanglingSinkCheckClean(t *testing.T) {
	t.Parallel()
	sink := t.TempDir()
	live := filepath.Join(t.TempDir(), "skill")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(live, filepath.Join(sink, "gc-work")); err != nil {
		t.Fatal(err)
	}
	c := NewSkillDanglingSinkCheck([]string{sink}, nil, nil)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %v, want OK (%s)", r.Status, r.Message)
	}
}

func TestSkillDanglingSinkCheckMissingSinkSkipped(t *testing.T) {
	t.Parallel()
	c := NewSkillDanglingSinkCheck([]string{filepath.Join(t.TempDir(), "no-such-sink")}, nil, nil)
	if r := c.Run(&CheckContext{}); r.Status != StatusOK {
		t.Fatalf("status = %v, want OK (%s)", r.Status, r.Message)
	}
}

func TestSkillDanglingSinkCheckFlagsAndClassifies(t *testing.T) {
	t.Parallel()
	sink := t.TempDir()
	legacyRoot := filepath.Join(t.TempDir(), ".gc", "system", "packs")
	userRoot := filepath.Join(t.TempDir(), "user")
	mkDanglingLink(t, sink, "core.gc-mail", filepath.Join(legacyRoot, "core", "skills", "gc-mail"))
	mkDanglingLink(t, sink, "mine", filepath.Join(userRoot, "mine"))

	c := NewSkillDanglingSinkCheck([]string{sink}, []string{legacyRoot}, nil)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %v, want warning", r.Status)
	}
	if r.Severity != SeverityAdvisory {
		t.Errorf("severity = %v, want advisory", r.Severity)
	}
	if !strings.Contains(r.Message, "2 dangling") || !strings.Contains(r.Message, "1 gc-owned") {
		t.Errorf("message = %q", r.Message)
	}
	if r.FixHint == "" {
		t.Error("FixHint empty with gc-owned dangling links present")
	}
}

func TestSkillDanglingSinkCheckFixRemovesOnlyGcOwned(t *testing.T) {
	t.Parallel()
	sink := t.TempDir()
	legacyRoot := filepath.Join(t.TempDir(), ".gc", "system", "packs")
	cacheRoot := filepath.Join(t.TempDir(), ".gc", "cache", "repos")
	userRoot := filepath.Join(t.TempDir(), "user")
	gcLegacy := filepath.Join(sink, "core.gc-mail")
	gcCache := filepath.Join(sink, "core.gc-work")
	userLink := filepath.Join(sink, "mine")
	mkDanglingLink(t, sink, "core.gc-mail", filepath.Join(legacyRoot, "core", "skills", "gc-mail"))
	mkDanglingLink(t, sink, "core.gc-work", filepath.Join(cacheRoot, "be555", "skills", "gc-work"))
	mkDanglingLink(t, sink, "mine", filepath.Join(userRoot, "mine"))

	c := NewSkillDanglingSinkCheck([]string{sink}, []string{legacyRoot, cacheRoot}, nil)
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{gcLegacy, gcCache} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("gc-owned dangling link survived fix: %s (err=%v)", p, err)
		}
	}
	if _, err := os.Lstat(userLink); err != nil {
		t.Errorf("user-owned link removed by fix: %v", err)
	}
	// Post-fix run reports clean for gc-owned; the user link remains
	// flagged but is not fixable.
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning || !strings.Contains(r.Message, "1 dangling") || !strings.Contains(r.Message, "0 gc-owned") {
		t.Errorf("post-fix result = %v %q", r.Status, r.Message)
	}
}

func TestSkillDanglingSinkCheckLiveSinksLazy(t *testing.T) {
	t.Parallel()
	staticSink := t.TempDir()
	liveSink := t.TempDir()
	legacyRoot := filepath.Join(t.TempDir(), ".gc", "system", "packs")
	mkDanglingLink(t, liveSink, "core.gc-city", filepath.Join(legacyRoot, "core", "skills", "gc-city"))

	calls := 0
	c := NewSkillDanglingSinkCheck([]string{staticSink}, []string{legacyRoot}, func() []string {
		calls++
		return []string{liveSink}
	})
	if calls != 0 {
		t.Fatal("liveSinksFn evaluated during construction")
	}
	r := c.Run(&CheckContext{})
	if calls != 1 {
		t.Fatalf("liveSinksFn called %d times, want 1", calls)
	}
	if r.Status != StatusWarning || !strings.Contains(r.Message, "1 dangling") {
		t.Fatalf("result = %v %q", r.Status, r.Message)
	}
}

func TestSkillDanglingSinkCheckDeduplicatesSinks(t *testing.T) {
	t.Parallel()
	sink := t.TempDir()
	legacyRoot := filepath.Join(t.TempDir(), ".gc", "system", "packs")
	mkDanglingLink(t, sink, "core.gc-mail", filepath.Join(legacyRoot, "core", "skills", "gc-mail"))

	// Same sink via static list and live list (scope root == session
	// workdir for stage-1-only agents) must report once.
	c := NewSkillDanglingSinkCheck([]string{sink}, []string{legacyRoot}, func() []string { return []string{sink} })
	r := c.Run(&CheckContext{})
	if !strings.Contains(r.Message, "1 dangling") {
		t.Fatalf("message = %q, want exactly one report", r.Message)
	}
}
