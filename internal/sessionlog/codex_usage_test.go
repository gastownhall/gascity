package sessionlog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCodexUsageLines writes raw JSONL lines to path, creating parents.
func writeCodexUsageLines(t *testing.T, path string, lines []string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

// codexTokenCountLine builds an event_msg token_count line mirroring the real
// rollout shape (cumulative total_token_usage plus per-call last_token_usage).
func codexTokenCountLine(ts string, total, lastInput, lastCached, lastOutput, lastReasoning int) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d,"reasoning_output_tokens":%d,"total_tokens":%d},"last_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d,"reasoning_output_tokens":%d,"total_tokens":%d},"model_context_window":258400},"rate_limits":{"limit_id":"codex","limit_name":null,"primary":{"used_percent":0.0,"window_minutes":300,"resets_at":1776394093},"secondary":{"used_percent":0.0,"window_minutes":10080,"resets_at":1776980893},"credits":null,"plan_type":"pro"}}}`,
		ts, total-lastOutput, lastCached, lastOutput, lastReasoning, total,
		lastInput, lastCached, lastOutput, lastReasoning, lastInput+lastOutput)
}

func codexTurnContextLine(ts, model string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"turn_id":"019d9845-45f6-70d2-86e8-53d8a44a830f","cwd":"/work/dir","current_date":"2026-04-16","timezone":"Etc/UTC","approval_policy":"never","sandbox_policy":{"type":"danger-full-access"},"model":%q,"personality":"pragmatic"}}`, ts, model)
}

func codexSessionMetaLine(ts, cwd string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":"019d9845-4273-7ee3-a7d7-15b71ec6f096","timestamp":%q,"cwd":%q,"originator":"codex-tui","cli_version":"0.121.0","source":"cli","model_provider":"openai"}}`, ts, ts, cwd)
}

const codexNullInfoTokenCountLine = `{"timestamp":"2026-04-16T21:49:31.051Z","type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"limit_id":"codex","limit_name":null,"primary":{"used_percent":0.0,"window_minutes":300,"resets_at":1776394093},"secondary":{"used_percent":0.0,"window_minutes":10080,"resets_at":1776980893},"credits":null,"plan_type":"pro"}}}`

func TestExtractCodexTailUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-2026-04-16T21-49-29-test.jsonl")
	writeCodexUsageLines(t, path, []string{
		codexSessionMetaLine("2026-04-16T21:49:30.734Z", "/work/dir"),
		codexTurnContextLine("2026-04-16T21:49:30.901Z", "gpt-5.4"),
		codexNullInfoTokenCountLine,
		codexTokenCountLine("2026-04-16T21:49:38.304Z", 15917, 15562, 10624, 355, 166),
		codexTurnContextLine("2026-04-16T21:49:40.000Z", "gpt-5.5"),
		codexTokenCountLine("2026-04-16T21:49:45.100Z", 34114, 17888, 15232, 309, 28),
		// Exact duplicate emission (same cumulative totals, later timestamp) —
		// a real artifact observed 470ms apart in host rollouts.
		codexTokenCountLine("2026-04-16T21:49:45.570Z", 34114, 17888, 15232, 309, 28),
		// All-zero per-call usage must be skipped even with a fresh total.
		codexTokenCountLine("2026-04-16T21:49:50.000Z", 99999, 0, 0, 0, 0),
		`{"timestamp":"2026-04-16T21:4`, // torn trailing line tolerated
	})

	usages, err := ExtractCodexTailUsage(path)
	if err != nil {
		t.Fatalf("ExtractCodexTailUsage: %v", err)
	}
	if len(usages) != 2 {
		t.Fatalf("got %d usages, want 2: %+v", len(usages), usages)
	}

	first := usages[0]
	if first.MessageID != "total:15917" {
		t.Errorf("first.MessageID = %q, want total:15917", first.MessageID)
	}
	if first.EntryUUID != "2026-04-16T21:49:38.304Z" {
		t.Errorf("first.EntryUUID = %q, want line timestamp", first.EntryUUID)
	}
	if first.Model != "gpt-5.4" {
		t.Errorf("first.Model = %q, want gpt-5.4 (latest preceding turn_context)", first.Model)
	}
	if first.InputTokens != 15562-10624 {
		t.Errorf("first.InputTokens = %d, want %d (input - cached)", first.InputTokens, 15562-10624)
	}
	if first.OutputTokens != 355 {
		t.Errorf("first.OutputTokens = %d, want 355 (reasoning is a subset, not added)", first.OutputTokens)
	}
	if first.CacheReadTokens != 10624 {
		t.Errorf("first.CacheReadTokens = %d, want 10624", first.CacheReadTokens)
	}
	if first.CacheCreationTokens != 0 {
		t.Errorf("first.CacheCreationTokens = %d, want 0", first.CacheCreationTokens)
	}

	second := usages[1]
	if second.MessageID != "total:34114" {
		t.Errorf("second.MessageID = %q, want total:34114", second.MessageID)
	}
	if second.EntryUUID != "2026-04-16T21:49:45.570Z" {
		t.Errorf("second.EntryUUID = %q, want the LAST duplicate's timestamp (collapse)", second.EntryUUID)
	}
	if second.Model != "gpt-5.5" {
		t.Errorf("second.Model = %q, want gpt-5.5 (model switched by later turn_context)", second.Model)
	}
	if second.InputTokens != 17888-15232 {
		t.Errorf("second.InputTokens = %d, want %d", second.InputTokens, 17888-15232)
	}
	if second.OutputTokens != 309 {
		t.Errorf("second.OutputTokens = %d, want 309", second.OutputTokens)
	}
	if second.CacheReadTokens != 15232 {
		t.Errorf("second.CacheReadTokens = %d, want 15232", second.CacheReadTokens)
	}
}

// TestExtractCodexTailUsageDuplicateKeepsFirstModel pins the real codex
// emission order around a model switch: the CLI re-emits the prior turn's
// final cumulative snapshot AFTER the new turn's turn_context, so the
// duplicate collapse must not relabel the already-observed invocation with
// the new turn's model.
func TestExtractCodexTailUsageDuplicateKeepsFirstModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-2026-04-16T21-49-29-modelswitch.jsonl")
	writeCodexUsageLines(t, path, []string{
		codexSessionMetaLine("2026-04-16T21:49:30.734Z", "/work/dir"),
		codexTurnContextLine("2026-04-16T21:49:30.901Z", "gpt-5.4"),
		codexTokenCountLine("2026-04-16T21:49:38.304Z", 15917, 15562, 10624, 355, 166),
		// Mid-session model switch, then the prior turn's final cumulative
		// snapshot re-emitted under the NEW turn_context.
		codexTurnContextLine("2026-04-16T21:49:40.000Z", "gpt-5.5"),
		codexTokenCountLine("2026-04-16T21:49:40.470Z", 15917, 15562, 10624, 355, 166),
		codexTokenCountLine("2026-04-16T21:49:45.100Z", 34114, 17888, 15232, 309, 28),
	})

	usages, err := ExtractCodexTailUsage(path)
	if err != nil {
		t.Fatalf("ExtractCodexTailUsage: %v", err)
	}
	if len(usages) != 2 {
		t.Fatalf("got %d usages, want 2: %+v", len(usages), usages)
	}
	if usages[0].Model != "gpt-5.4" {
		t.Errorf("first.Model = %q, want gpt-5.4 (duplicate re-emission must not relabel)", usages[0].Model)
	}
	if usages[0].EntryUUID != "2026-04-16T21:49:40.470Z" {
		t.Errorf("first.EntryUUID = %q, want the last duplicate's timestamp (collapse still refreshes the rest)", usages[0].EntryUUID)
	}
	if usages[1].Model != "gpt-5.5" {
		t.Errorf("second.Model = %q, want gpt-5.5 (new invocation under the new turn_context)", usages[1].Model)
	}
}

func TestExtractCodexTailUsageClampsNegativeInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-2026-04-16T21-49-29-clamp.jsonl")
	writeCodexUsageLines(t, path, []string{
		codexSessionMetaLine("2026-04-16T21:49:30.734Z", "/work/dir"),
		codexTurnContextLine("2026-04-16T21:49:30.901Z", "gpt-5.5"),
		// cached_input_tokens exceeding input_tokens must clamp to zero, not
		// go negative.
		codexTokenCountLine("2026-04-16T21:49:38.304Z", 500, 100, 400, 50, 0),
	})

	usages, err := ExtractCodexTailUsage(path)
	if err != nil {
		t.Fatalf("ExtractCodexTailUsage: %v", err)
	}
	if len(usages) != 1 {
		t.Fatalf("got %d usages, want 1", len(usages))
	}
	if usages[0].InputTokens != 0 {
		t.Errorf("InputTokens = %d, want 0 (clamped)", usages[0].InputTokens)
	}
	if usages[0].CacheReadTokens != 400 {
		t.Errorf("CacheReadTokens = %d, want 400", usages[0].CacheReadTokens)
	}
}

func TestExtractCodexTailUsageModelMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-2026-04-16T21-49-29-nomodel.jsonl")
	writeCodexUsageLines(t, path, []string{
		codexSessionMetaLine("2026-04-16T21:49:30.734Z", "/work/dir"),
		// No turn_context in the window: tokens still flow, model is empty
		// (cost is skipped upstream).
		codexTokenCountLine("2026-04-16T21:49:38.304Z", 15917, 15562, 10624, 355, 166),
	})

	usages, err := ExtractCodexTailUsage(path)
	if err != nil {
		t.Fatalf("ExtractCodexTailUsage: %v", err)
	}
	if len(usages) != 1 {
		t.Fatalf("got %d usages, want 1", len(usages))
	}
	if usages[0].Model != "" {
		t.Errorf("Model = %q, want empty when no turn_context precedes", usages[0].Model)
	}
	if usages[0].InputTokens != 15562-10624 {
		t.Errorf("InputTokens = %d, want %d", usages[0].InputTokens, 15562-10624)
	}
}

func TestExtractCodexTailUsageFromSearchPaths(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "2026", "04", "16", "rollout-2026-04-16T21-49-29-in.jsonl")
	writeCodexUsageLines(t, inside, []string{
		codexSessionMetaLine("2026-04-16T21:49:30.734Z", "/work/dir"),
		codexTurnContextLine("2026-04-16T21:49:30.901Z", "gpt-5.5"),
		codexTokenCountLine("2026-04-16T21:49:38.304Z", 15917, 15562, 10624, 355, 166),
	})

	usages, err := ExtractCodexTailUsageFromSearchPaths([]string{root}, inside)
	if err != nil {
		t.Fatalf("ExtractCodexTailUsageFromSearchPaths(inside root): %v", err)
	}
	if len(usages) != 1 {
		t.Fatalf("got %d usages, want 1", len(usages))
	}

	outside := filepath.Join(t.TempDir(), "rollout-2026-04-16T21-49-29-out.jsonl")
	writeCodexUsageLines(t, outside, []string{
		codexSessionMetaLine("2026-04-16T21:49:30.734Z", "/work/dir"),
	})
	if _, err := ExtractCodexTailUsageFromSearchPaths([]string{root}, outside); err == nil {
		t.Fatal("path outside all merged codex roots must be rejected")
	}
}

func TestFindCodexSessionFileByID(t *testing.T) {
	workDir := "/work/by-id-discovery"
	// Synthetic uuid: must never collide with a real rollout under the
	// merged default root (~/.codex/sessions) on a developer machine.
	const uuid = "019e9966-aaaa-7000-8000-26a2dd7e15b3"
	now := time.Date(2026, 6, 10, 14, 30, 0, 0, time.Local)

	t.Run("found in an old day dir several days before notAfter", func(t *testing.T) {
		root := t.TempDir()
		// Resumed sessions append to the ORIGINAL rollout whose filename
		// timestamp is days old; the keyed lookup must still find it.
		want := writeCodexRolloutAt(t, root, now.Add(-5*24*time.Hour), uuid, workDir)
		got := FindCodexSessionFileByID([]string{root}, workDir, uuid, now.Add(-6*24*time.Hour), now)
		if got != want {
			t.Fatalf("FindCodexSessionFileByID = %q, want %q", got, want)
		}
	})

	t.Run("cwd mismatch refused", func(t *testing.T) {
		root := t.TempDir()
		writeCodexRolloutAt(t, root, now.Add(-time.Hour), uuid, "/some/other/dir")
		if got := FindCodexSessionFileByID([]string{root}, workDir, uuid, now.Add(-24*time.Hour), now); got != "" {
			t.Fatalf("FindCodexSessionFileByID = %q, want empty (session_meta cwd mismatch)", got)
		}
	})

	t.Run("absent returns empty", func(t *testing.T) {
		root := t.TempDir()
		writeCodexRolloutAt(t, root, now.Add(-time.Hour), "019e9966-ffff-7000-8000-000000000099", workDir)
		if got := FindCodexSessionFileByID([]string{root}, workDir, uuid, now.Add(-24*time.Hour), now); got != "" {
			t.Fatalf("FindCodexSessionFileByID = %q, want empty (no rollout with the session id suffix)", got)
		}
	})

	t.Run("reachable via symlinked extra root", func(t *testing.T) {
		root := t.TempDir()
		target := t.TempDir() // aimux-managed account store outside the root
		if err := os.Symlink(target, filepath.Join(root, "aimux-acct")); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
		writeCodexRolloutAt(t, target, now.Add(-3*time.Hour), uuid, workDir)
		got := FindCodexSessionFileByID([]string{root}, workDir, uuid, now.Add(-24*time.Hour), now)
		if got == "" {
			t.Fatal("FindCodexSessionFileByID = empty, want rollout behind symlinked extra root")
		}
		// Must stay symlink-LEXICAL so the paired extractor's containment
		// validation against the merged roots accepts it.
		if !strings.HasPrefix(got, root+string(filepath.Separator)) {
			t.Errorf("FindCodexSessionFileByID = %q, want symlink-lexical path under root %q", got, root)
		}
	})

	t.Run("same physical file via symlinked and direct roots is not ambiguous", func(t *testing.T) {
		root := t.TempDir()
		target := t.TempDir()
		if err := os.Symlink(target, filepath.Join(root, "aimux-acct")); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
		writeCodexRolloutAt(t, target, now.Add(-3*time.Hour), uuid, workDir)
		got := FindCodexSessionFileByID([]string{root, target}, workDir, uuid, now.Add(-24*time.Hour), now)
		if got == "" {
			t.Fatal("FindCodexSessionFileByID = empty, want the single physical rollout (symlink alias must not refuse as ambiguous)")
		}
	})

	t.Run("two distinct physical matches refused", func(t *testing.T) {
		root := t.TempDir()
		writeCodexRolloutAt(t, root, now.Add(-time.Hour), uuid, workDir)
		writeCodexRolloutAt(t, root, now.Add(-26*time.Hour), uuid, workDir)
		if got := FindCodexSessionFileByID([]string{root}, workDir, uuid, now.Add(-48*time.Hour), now); got != "" {
			t.Fatalf("FindCodexSessionFileByID = %q, want empty (two distinct physical files share the suffix)", got)
		}
	})

	t.Run("empty inputs refused", func(t *testing.T) {
		root := t.TempDir()
		writeCodexRolloutAt(t, root, now.Add(-time.Hour), uuid, workDir)
		if got := FindCodexSessionFileByID([]string{root}, "", uuid, now.Add(-24*time.Hour), now); got != "" {
			t.Fatalf("empty workDir: got %q, want empty", got)
		}
		if got := FindCodexSessionFileByID([]string{root}, workDir, "", now.Add(-24*time.Hour), now); got != "" {
			t.Fatalf("empty sessionID: got %q, want empty", got)
		}
		if got := FindCodexSessionFileByID([]string{root}, workDir, uuid, now.Add(-24*time.Hour), time.Time{}); got != "" {
			t.Fatalf("zero notAfter: got %q, want empty", got)
		}
	})
}

// writeCodexRolloutAt creates a minimal rollout in the local-date day tree the
// way the codex CLI does: filename timestamp in LOCAL time, session_meta cwd
// on the first line.
func writeCodexRolloutAt(t *testing.T, root string, ts time.Time, uuid, cwd string) string {
	t.Helper()
	local := ts.In(time.Local)
	dir := filepath.Join(root, local.Format("2006"), local.Format("01"), local.Format("02"))
	path := filepath.Join(dir, "rollout-"+local.Format("2006-01-02T15-04-05")+"-"+uuid+".jsonl")
	writeCodexUsageLines(t, path, []string{
		codexSessionMetaLine(ts.UTC().Format(time.RFC3339Nano), cwd),
	})
	return path
}

func TestFindCodexSessionFileNear(t *testing.T) {
	anchor := time.Date(2026, 6, 10, 14, 30, 0, 0, time.Local)
	window := 10 * time.Minute
	workDir := "/work/near-discovery"

	t.Run("in-window cwd match found", func(t *testing.T) {
		root := t.TempDir()
		want := writeCodexRolloutAt(t, root, anchor.Add(2*time.Minute), "019d9845-aaaa-7000-8000-000000000001", workDir)
		got := FindCodexSessionFileNear([]string{root}, workDir, anchor, window)
		if got != want {
			t.Fatalf("FindCodexSessionFileNear = %q, want %q", got, want)
		}
	})

	t.Run("out-of-window timestamp refused even with matching cwd", func(t *testing.T) {
		root := t.TempDir()
		writeCodexRolloutAt(t, root, anchor.Add(-48*time.Hour), "019d9845-aaaa-7000-8000-000000000002", workDir)
		if got := FindCodexSessionFileNear([]string{root}, workDir, anchor, window); got != "" {
			t.Fatalf("FindCodexSessionFileNear = %q, want empty (timestamp outside window)", got)
		}
	})

	t.Run("in-window with different cwd refused", func(t *testing.T) {
		root := t.TempDir()
		writeCodexRolloutAt(t, root, anchor.Add(time.Minute), "019d9845-aaaa-7000-8000-000000000003", "/some/other/dir")
		if got := FindCodexSessionFileNear([]string{root}, workDir, anchor, window); got != "" {
			t.Fatalf("FindCodexSessionFileNear = %q, want empty (cwd mismatch)", got)
		}
	})

	t.Run("two in-window matches refused as ambiguous", func(t *testing.T) {
		root := t.TempDir()
		writeCodexRolloutAt(t, root, anchor.Add(time.Minute), "019d9845-aaaa-7000-8000-000000000004", workDir)
		writeCodexRolloutAt(t, root, anchor.Add(3*time.Minute), "019d9845-aaaa-7000-8000-000000000005", workDir)
		if got := FindCodexSessionFileNear([]string{root}, workDir, anchor, window); got != "" {
			t.Fatalf("FindCodexSessionFileNear = %q, want empty (ambiguity refusal)", got)
		}
	})

	t.Run("window spanning midnight finds next-day file", func(t *testing.T) {
		root := t.TempDir()
		midnightAnchor := time.Date(2026, 6, 10, 23, 58, 0, 0, time.Local)
		want := writeCodexRolloutAt(t, root, midnightAnchor.Add(5*time.Minute), "019d9845-aaaa-7000-8000-000000000006", workDir)
		got := FindCodexSessionFileNear([]string{root}, workDir, midnightAnchor, window)
		if got != want {
			t.Fatalf("FindCodexSessionFileNear = %q, want %q (next local day dir)", got, want)
		}
	})

	t.Run("symlinked extra root yields extractable lexical path", func(t *testing.T) {
		root := t.TempDir()
		target := t.TempDir() // aimux-managed account store outside the root
		if err := os.Symlink(target, filepath.Join(root, "aimux-acct")); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
		ts := anchor.Add(2 * time.Minute)
		local := ts.In(time.Local)
		name := "rollout-" + local.Format("2006-01-02T15-04-05") + "-019d9845-aaaa-7000-8000-000000000008.jsonl"
		writeCodexUsageLines(t,
			filepath.Join(target, local.Format("2006"), local.Format("01"), local.Format("02"), name),
			[]string{
				codexSessionMetaLine(ts.UTC().Format(time.RFC3339Nano), workDir),
				codexTurnContextLine(ts.UTC().Format(time.RFC3339Nano), "gpt-5.5"),
				codexTokenCountLine(ts.UTC().Format(time.RFC3339Nano), 15917, 15562, 10624, 355, 166),
			})

		got := FindCodexSessionFileNear([]string{root}, workDir, anchor, window)
		if got == "" {
			t.Fatal("FindCodexSessionFileNear = empty, want rollout behind symlinked extra root")
		}
		// The discovered path must stay symlink-LEXICAL (under root, through
		// the link) — never the EvalSymlinks-resolved target — because the
		// paired extractor validates containment lexically against the merged
		// search roots; a resolved path is rejected there and the session
		// silently records zero tokens forever.
		if !strings.HasPrefix(got, root+string(filepath.Separator)) {
			t.Errorf("FindCodexSessionFileNear = %q, want symlink-lexical path under root %q", got, root)
		}
		usages, err := ExtractCodexTailUsageFromSearchPaths([]string{root}, got)
		if err != nil {
			t.Fatalf("ExtractCodexTailUsageFromSearchPaths(discovered path): %v", err)
		}
		if len(usages) != 1 {
			t.Fatalf("got %d usages, want 1", len(usages))
		}
	})

	t.Run("within forward DST tolerance accepted", func(t *testing.T) {
		root := t.TempDir()
		want := writeCodexRolloutAt(t, root, anchor.Add(window+30*time.Minute), "019d9845-aaaa-7000-8000-00000000000a", workDir)
		got := FindCodexSessionFileNear([]string{root}, workDir, anchor, window)
		if got != want {
			t.Fatalf("FindCodexSessionFileNear = %q, want %q (inside the +1h fold tolerance)", got, want)
		}
	})

	t.Run("within backward DST tolerance accepted", func(t *testing.T) {
		root := t.TempDir()
		want := writeCodexRolloutAt(t, root, anchor.Add(-30*time.Minute), "019d9845-aaaa-7000-8000-00000000000b", workDir)
		got := FindCodexSessionFileNear([]string{root}, workDir, anchor, window)
		if got != want {
			t.Fatalf("FindCodexSessionFileNear = %q, want %q (inside the -1h fold tolerance)", got, want)
		}
	})

	t.Run("beyond forward tolerance refused", func(t *testing.T) {
		root := t.TempDir()
		// Same local day as the anchor so the rollout reaches the timestamp
		// filter (the -48h fixture above never does — its day dir is outside
		// the scanned range).
		writeCodexRolloutAt(t, root, anchor.Add(window+2*time.Hour), "019d9845-aaaa-7000-8000-00000000000c", workDir)
		if got := FindCodexSessionFileNear([]string{root}, workDir, anchor, window); got != "" {
			t.Fatalf("FindCodexSessionFileNear = %q, want empty (beyond end+1h tolerance)", got)
		}
	})

	t.Run("before backward tolerance refused", func(t *testing.T) {
		root := t.TempDir()
		writeCodexRolloutAt(t, root, anchor.Add(-2*time.Hour), "019d9845-aaaa-7000-8000-00000000000d", workDir)
		if got := FindCodexSessionFileNear([]string{root}, workDir, anchor, window); got != "" {
			t.Fatalf("FindCodexSessionFileNear = %q, want empty (before start-1h tolerance)", got)
		}
	})

	t.Run("forward tolerance across midnight finds next-day file", func(t *testing.T) {
		root := t.TempDir()
		lateAnchor := time.Date(2026, 6, 10, 23, 35, 0, 0, time.Local)
		// end+1h crosses local midnight: the next-day dir must be scanned.
		want := writeCodexRolloutAt(t, root, lateAnchor.Add(window+45*time.Minute), "019d9845-aaaa-7000-8000-00000000000e", workDir)
		got := FindCodexSessionFileNear([]string{root}, workDir, lateAnchor, window)
		if got != want {
			t.Fatalf("FindCodexSessionFileNear = %q, want %q (next-day dir inside +1h tolerance)", got, want)
		}
	})

	t.Run("backward tolerance across midnight finds previous-day file", func(t *testing.T) {
		root := t.TempDir()
		earlyAnchor := time.Date(2026, 6, 11, 0, 20, 0, 0, time.Local)
		// start-1h crosses local midnight backward: the previous-day dir must
		// be scanned.
		want := writeCodexRolloutAt(t, root, earlyAnchor.Add(-30*time.Minute), "019d9845-aaaa-7000-8000-00000000000f", workDir)
		got := FindCodexSessionFileNear([]string{root}, workDir, earlyAnchor, window)
		if got != want {
			t.Fatalf("FindCodexSessionFileNear = %q, want %q (previous-day dir inside -1h tolerance)", got, want)
		}
	})

	t.Run("same physical rollout via symlinked and direct roots is not ambiguous", func(t *testing.T) {
		root := t.TempDir()
		target := t.TempDir() // account store reachable both ways
		if err := os.Symlink(target, filepath.Join(root, "aimux-acct")); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
		writeCodexRolloutAt(t, target, anchor.Add(2*time.Minute), "019d9845-aaaa-7000-8000-000000000009", workDir)

		// One physical file, two lexical paths (root/aimux-acct/... and
		// target/...): physical-identity dedup must keep it a single match.
		got := FindCodexSessionFileNear([]string{root, target}, workDir, anchor, window)
		if got == "" {
			t.Fatal("FindCodexSessionFileNear = empty, want the single physical rollout (symlink alias must not refuse as ambiguous)")
		}
	})

	t.Run("missing roots return empty", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "does-not-exist")
		if got := FindCodexSessionFileNear([]string{root}, workDir, anchor, window); got != "" {
			t.Fatalf("FindCodexSessionFileNear = %q, want empty", got)
		}
	})

	t.Run("zero anchor or window refused", func(t *testing.T) {
		root := t.TempDir()
		writeCodexRolloutAt(t, root, anchor.Add(time.Minute), "019d9845-aaaa-7000-8000-000000000007", workDir)
		if got := FindCodexSessionFileNear([]string{root}, workDir, time.Time{}, window); got != "" {
			t.Fatalf("zero anchor: got %q, want empty", got)
		}
		if got := FindCodexSessionFileNear([]string{root}, workDir, anchor, 0); got != "" {
			t.Fatalf("zero window: got %q, want empty", got)
		}
	})
}
