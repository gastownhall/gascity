package sessionlog

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// canonical-path migration coverage (refs ga-iawy13.5).
//
// appendCodexRolloutMatch and appendCodexCandidatesFromDir compute their
// dedup identity via a bare filepath.EvalSymlinks with no Abs() step. A
// relative-target symlink preserves relative-ness through resolution (per
// filepath.EvalSymlinks' documented semantics), so the same physical rollout
// reached once through an absolute path and once through a relative alias
// produces two different dedup keys and is wrongly counted twice.

func TestAppendCodexRolloutMatchDedupsRelativeAndAbsolutePaths(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "rollout-target.jsonl")
	if err := os.WriteFile(target, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("rollout-target.jsonl", filepath.Join(dir, "rollout-alias.jsonl")); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}
	t.Chdir(dir)

	seen := make(map[string]bool)
	var matches []string
	appendCodexRolloutMatch(target, seen, &matches)
	appendCodexRolloutMatch("rollout-alias.jsonl", seen, &matches)

	if len(matches) != 1 {
		t.Fatalf("matches = %v, want a single deduped entry for the same physical rollout (got %d)", matches, len(matches))
	}
}

func TestAppendCodexCandidatesFromDirDedupsRelativeAndAbsoluteRoots(t *testing.T) {
	base := t.TempDir()
	workDir := "/data/projects/myproject"
	realDir := filepath.Join(base, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"type":"session_meta","payload":{"cwd":%q}}`, workDir)
	if err := os.WriteFile(filepath.Join(realDir, "rollout-a.jsonl"), []byte(meta+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(base, "alias")); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}
	t.Chdir(base)

	seen := make(map[string]bool)
	var out []CodexSessionCandidate
	appendCodexCandidatesFromDir(realDir, workDir, seen, &out)
	appendCodexCandidatesFromDir("alias", workDir, seen, &out)

	if len(out) != 1 {
		t.Fatalf("out = %+v, want a single deduped candidate for the same physical rollout (got %d)", out, len(out))
	}
}

// Bare-site regression coverage (refs ga-iawy13.5): findCodexSessionFileIn,
// findCodexRolloutBySuffixIn, and collectCodexCandidatesInDays resolve a
// symlinked extraRoot to actually recurse into it (a deliberate
// existence/resolvability check, not comparison prep), skipping on error. A
// dangling sibling root must not abort the scan or leak an error — it must
// simply be skipped while a later, valid root still resolves.

func TestFindCodexSessionFileInSkipsBrokenSymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	workDir := "/data/projects/myproject"

	if err := os.Symlink(filepath.Join(base, "does-not-exist"), filepath.Join(base, "account-a-broken")); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}

	validRoot := t.TempDir()
	dayDir := filepath.Join(validRoot, "2026", "01", "25")
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dayDir, "rollout-valid.jsonl")
	meta := fmt.Sprintf(`{"type":"session_meta","payload":{"cwd":%q}}`, workDir)
	if err := os.WriteFile(want, []byte(meta+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(validRoot, filepath.Join(base, "account-b-valid")); err != nil {
		t.Fatal(err)
	}

	if got := findCodexSessionFileIn(base, workDir); got != want {
		t.Fatalf("findCodexSessionFileIn() = %q, want %q (broken sibling extraRoot must be skipped, not fatal)", got, want)
	}
}

func TestFindCodexRolloutBySuffixInSkipsBrokenSymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	workDir := "/data/projects/myproject"

	if err := os.Symlink(filepath.Join(base, "does-not-exist"), filepath.Join(base, "account-a-broken")); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}

	validRoot := t.TempDir()
	dayDir := filepath.Join(validRoot, "2026", "05", "19")
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	targetID := "019e3e8e-3591-7532-a1ef-8b9e882bea2f"
	want := filepath.Join(dayDir, "rollout-2026-05-19T04-46-07-"+targetID+".jsonl")
	meta := fmt.Sprintf(`{"type":"session_meta","payload":{"cwd":%q}}`, workDir)
	if err := os.WriteFile(want, []byte(meta+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(validRoot, filepath.Join(base, "account-b-valid")); err != nil {
		t.Fatal(err)
	}

	if got := FindCodexSessionFileByIDNoWindow([]string{base}, workDir, targetID); got != want {
		t.Fatalf("FindCodexSessionFileByIDNoWindow() = %q, want %q (broken sibling extraRoot must be skipped, not fatal)", got, want)
	}
}

func TestCollectCodexCandidatesInDaysSkipsBrokenSymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	workDir := "/data/projects/myproject"

	if err := os.Symlink(filepath.Join(base, "does-not-exist"), filepath.Join(base, "account-a-broken")); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}

	validRoot := t.TempDir()
	start := time.Date(2026, 5, 19, 4, 46, 7, 0, time.UTC)
	dayDir := filepath.Join(validRoot, "2026", "05", "19")
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dayDir, "rollout-2026-05-19T04-46-07-019e3e8e-3591-7532-a1ef-8b9e882bea2f.jsonl")
	meta := fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"cwd":%q}}`, start.Format(time.RFC3339), workDir)
	if err := os.WriteFile(want, []byte(meta+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(validRoot, filepath.Join(base, "account-b-valid")); err != nil {
		t.Fatal(err)
	}

	if got := FindCodexSessionFileInTimeWindow([]string{base}, workDir, start, time.Time{}); got != want {
		t.Fatalf("FindCodexSessionFileInTimeWindow() = %q, want %q (broken sibling extraRoot must be skipped, not fatal)", got, want)
	}
}
