package sessionlog

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeCodexRolloutWithMeta writes a rollout whose session_meta payload carries
// extra fields beyond the cwd, so tests can mark a rollout as a child thread
// (a non-"cli" source or a parent_thread_id) the way codex sub-agent rollouts do.
func writeCodexRolloutWithMeta(t *testing.T, root string, ts time.Time, uuid, cwd, extraPayload string) string {
	t.Helper()
	local := ts.In(time.Local)
	dir := filepath.Join(root, local.Format("2006"), local.Format("01"), local.Format("02"))
	path := filepath.Join(dir, "rollout-"+local.Format("2006-01-02T15-04-05")+"-"+uuid+".jsonl")
	meta := fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":%q,"cwd":%q%s}}`,
		ts.UTC().Format(time.RFC3339Nano), uuid, cwd, extraPayload)
	writeCodexUsageLines(t, path, []string{meta})
	return path
}

// TestFindCodexSessionFileNearScanTypedResults pins the typed near-scan
// verdicts a live-lane caller needs to tell apart: a CLEAN zero-match (a
// rollout may still appear), an AMBIGUOUS refusal (two roots, definitive),
// a dirty scan (transient IO fault, non-definitive), and a clean singleton.
func TestFindCodexSessionFileNearScanTypedResults(t *testing.T) {
	anchor := time.Date(2026, 6, 10, 14, 30, 0, 0, time.Local)
	window := 10 * time.Minute
	workDir := "/work/typed-near-scan"

	t.Run("clean zero match", func(t *testing.T) {
		root := t.TempDir()
		got := FindCodexSessionFileNearScan([]string{root}, workDir, anchor, window)
		if got.Path != "" || !got.Clean || got.Ambiguous {
			t.Fatalf("empty root: got %+v, want {Path:\"\", Clean:true, Ambiguous:false}", got)
		}
	})

	t.Run("bad inputs are a clean no-op", func(t *testing.T) {
		got := FindCodexSessionFileNearScan([]string{t.TempDir()}, "", anchor, window)
		if got.Path != "" || !got.Clean || got.Ambiguous {
			t.Fatalf("no workdir: got %+v, want clean no-op", got)
		}
		got = FindCodexSessionFileNearScan([]string{t.TempDir()}, workDir, time.Time{}, window)
		if got.Path != "" || !got.Clean || got.Ambiguous {
			t.Fatalf("zero anchor: got %+v, want clean no-op", got)
		}
	})

	t.Run("ambiguous refusal is definitive", func(t *testing.T) {
		root := t.TempDir()
		writeCodexRolloutAt(t, root, anchor.Add(time.Minute), "019d9845-aaaa-7000-8000-000000000011", workDir)
		writeCodexRolloutAt(t, root, anchor.Add(3*time.Minute), "019d9845-aaaa-7000-8000-000000000012", workDir)
		got := FindCodexSessionFileNearScan([]string{root}, workDir, anchor, window)
		if got.Path != "" || !got.Ambiguous {
			t.Fatalf("two roots: got %+v, want {Path:\"\", Ambiguous:true}", got)
		}
		if !got.Clean {
			t.Fatalf("ambiguity is definitive regardless of noise: got %+v, want Clean:true", got)
		}
	})

	t.Run("clean singleton", func(t *testing.T) {
		root := t.TempDir()
		want := writeCodexRolloutAt(t, root, anchor.Add(2*time.Minute), "019d9845-aaaa-7000-8000-000000000013", workDir)
		got := FindCodexSessionFileNearScan([]string{root}, workDir, anchor, window)
		if got.Path != want || !got.Clean || got.Ambiguous {
			t.Fatalf("singleton: got %+v, want {Path:%q, Clean:true}", got, want)
		}
	})
}

// TestUnkeyedCodexDiscoveryExcludesChildRollouts pins ga-amrzyf case 2's
// discovery half: a rollout whose session_meta marks it as a child thread (a
// non-"cli" source, or a parent_thread_id) is never selected as a root
// candidate by the unkeyed cwd predicate — the near-scan window and the
// newest-first fallback both skip it, and two children with no root are a
// clean zero-match rather than an attribution. The exact keyed lookup is
// unchanged: a child rollout is still found by its own session id.
func TestUnkeyedCodexDiscoveryExcludesChildRollouts(t *testing.T) {
	anchor := time.Date(2026, 6, 10, 14, 30, 0, 0, time.Local)
	window := 10 * time.Minute
	workDir := "/work/child-exclusion"
	root := t.TempDir()

	t.Run("near scan selects the root among marked children", func(t *testing.T) {
		want := writeCodexRolloutAt(t, root, anchor.Add(time.Minute), "019d9845-bbbb-7000-8000-000000000021", workDir)
		writeCodexRolloutWithMeta(t, root, anchor.Add(2*time.Minute), "019d9845-bbbb-7000-8000-000000000022", workDir, `,"source":"thread"`)
		writeCodexRolloutWithMeta(t, root, anchor.Add(3*time.Minute), "019d9845-bbbb-7000-8000-000000000023", workDir, `,"parent_thread_id":"019d9845-bbbb-7000-8000-000000000021"`)
		got := FindCodexSessionFileNear([]string{root}, workDir, anchor, window)
		if got != want {
			t.Fatalf("FindCodexSessionFileNear = %q, want the root rollout %q (children excluded)", got, want)
		}
	})

	t.Run("near scan is a clean zero-match when only children exist", func(t *testing.T) {
		other := t.TempDir()
		writeCodexRolloutWithMeta(t, other, anchor.Add(time.Minute), "019d9845-bbbb-7000-8000-000000000024", workDir, `,"source":"sub_agent"`)
		writeCodexRolloutWithMeta(t, other, anchor.Add(2*time.Minute), "019d9845-bbbb-7000-8000-000000000025", workDir, `,"source":"sub_agent"`)
		got := FindCodexSessionFileNearScan([]string{other}, workDir, anchor, window)
		if got.Path != "" || !got.Clean || got.Ambiguous {
			t.Fatalf("children only: got %+v, want clean zero-match (children never make ambiguity)", got)
		}
	})

	t.Run("newest-first fallback skips children", func(t *testing.T) {
		// The full-tree fallback (Manager.TranscriptPath's codex route) must
		// pick the root even when a child is newer.
		want := writeCodexRolloutAt(t, root, anchor.Add(time.Minute), "019d9845-bbbb-7000-8000-000000000021", workDir)
		child := writeCodexRolloutWithMeta(t, root, anchor.Add(9*time.Minute), "019d9845-bbbb-7000-8000-000000000026", workDir, `,"source":"thread"`)
		if err := os.Chtimes(want, anchor.Add(30*time.Minute), anchor.Add(30*time.Minute)); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
		if err := os.Chtimes(filepath.Dir(want), anchor.Add(30*time.Minute), anchor.Add(30*time.Minute)); err != nil {
			t.Fatalf("Chtimes dir: %v", err)
		}
		// Make the child newer than the root so the mod-time ordering would
		// pick the child without exclusion.
		if err := os.Chtimes(child, anchor.Add(time.Hour), anchor.Add(time.Hour)); err != nil {
			t.Fatalf("Chtimes child: %v", err)
		}
		if got := FindCodexSessionFile([]string{root}, workDir); got != want {
			t.Fatalf("FindCodexSessionFile = %q, want root %q (newest child excluded)", got, want)
		}
	})

	t.Run("keyed lookup still resolves a child by its own id", func(t *testing.T) {
		other := t.TempDir()
		childID := "019d9845-bbbb-7000-8000-000000000027"
		writeCodexRolloutWithMeta(t, other, anchor.Add(time.Minute), childID, workDir, `,"parent_thread_id":"019d9845-bbbb-7000-8000-000000000021"`)
		got := FindCodexSessionFileByIDNoWindow([]string{other}, workDir, childID)
		if got == "" {
			t.Fatal("FindCodexSessionFileByIDNoWindow must still resolve a child rollout by its exact id (keyed lookup unchanged)")
		}
	})
}
