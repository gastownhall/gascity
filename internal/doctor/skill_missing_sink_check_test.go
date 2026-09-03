package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mkSinkEntry(t *testing.T, sink, name string) {
	t.Helper()
	if err := os.MkdirAll(sink, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "skill")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(sink, name)); err != nil {
		t.Fatal(err)
	}
}

func TestSkillMissingSinkCheckNilCandidatesFn(t *testing.T) {
	t.Parallel()
	c := NewSkillMissingSinkCheck(nil)
	if r := c.Run(&CheckContext{}); r.Status != StatusOK {
		t.Fatalf("status = %v, want OK (%s)", r.Status, r.Message)
	}
}

func TestSkillMissingSinkCheckNoCandidates(t *testing.T) {
	t.Parallel()
	c := NewSkillMissingSinkCheck(func() []SkillMissingSinkCandidate { return nil })
	if r := c.Run(&CheckContext{}); r.Status != StatusOK {
		t.Fatalf("status = %v, want OK (%s)", r.Status, r.Message)
	}
}

func TestSkillMissingSinkCheckScopeSinkEmptyIsNotFlagged(t *testing.T) {
	t.Parallel()
	scope := filepath.Join(t.TempDir(), "no-such-scope-sink")
	sess := filepath.Join(t.TempDir(), "no-such-session-sink")
	c := NewSkillMissingSinkCheck(func() []SkillMissingSinkCandidate {
		return []SkillMissingSinkCandidate{{SessionName: "s1", SessionSink: sess, ScopeSink: scope}}
	})
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %v, want OK when the agent has nothing materialized to begin with (%s)", r.Status, r.Message)
	}
}

func TestSkillMissingSinkCheckFlagsMissingSessionSink(t *testing.T) {
	t.Parallel()
	scope := t.TempDir()
	mkSinkEntry(t, scope, "core.gc-work")
	sess := filepath.Join(t.TempDir(), "worktree-with-no-sink")

	c := NewSkillMissingSinkCheck(func() []SkillMissingSinkCandidate {
		return []SkillMissingSinkCandidate{{SessionName: "worker-1", SessionSink: sess, ScopeSink: scope}}
	})
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %v, want warning (%s)", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Errorf("severity = %v, want advisory", r.Severity)
	}
	if !strings.Contains(r.Message, "1 live session") {
		t.Errorf("message = %q", r.Message)
	}
	if len(r.Details) != 1 || !strings.Contains(r.Details[0], "worker-1") {
		t.Errorf("details = %v, want the flagged session named", r.Details)
	}
	if r.FixHint == "" {
		t.Error("FixHint empty with a missing session sink present")
	}
	if c.CanFix() {
		t.Error("CanFix = true, want false")
	}
}

func TestSkillMissingSinkCheckMaterializedSessionSinkIsClean(t *testing.T) {
	t.Parallel()
	scope := t.TempDir()
	mkSinkEntry(t, scope, "core.gc-work")
	sess := t.TempDir()
	mkSinkEntry(t, sess, "core.gc-work")

	c := NewSkillMissingSinkCheck(func() []SkillMissingSinkCandidate {
		return []SkillMissingSinkCandidate{{SessionName: "worker-1", SessionSink: sess, ScopeSink: scope}}
	})
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %v, want OK (%s)", r.Status, r.Message)
	}
}

func TestSkillMissingSinkCheckCandidatesFnLazy(t *testing.T) {
	t.Parallel()
	calls := 0
	c := NewSkillMissingSinkCheck(func() []SkillMissingSinkCandidate {
		calls++
		return nil
	})
	if calls != 0 {
		t.Fatal("candidatesFn evaluated during construction")
	}
	c.Run(&CheckContext{})
	if calls != 1 {
		t.Fatalf("candidatesFn called %d times, want 1", calls)
	}
}
