package sessionlog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// codexPageFillerLine is a tool-output record of roughly 4 KiB, used to push a
// fixture past the fixed 64 KiB tail window the page reader replaces.
func codexPageFillerLine(ts string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"response_item","payload":{"type":"function_call_output","output":%q}}`,
		ts, strings.Repeat("x", 4096))
}

// drainCodexUsagePages loops ReadCodexUsagePage from resume (nil for a full
// replay) with the injected budgets until the source is drained, returning the
// concatenated usages and every page, so a test can assert per-pass shape.
func drainCodexUsagePages(t *testing.T, path string, resume *CodexUsageCheckpoint, budgets CodexUsagePageBudgets) ([]TailUsage, []CodexUsagePage) {
	t.Helper()
	var usages []TailUsage
	var pages []CodexUsagePage
	for range 64 {
		page, err := ReadCodexUsagePage(context.Background(), path, resume, budgets)
		if err != nil {
			t.Fatalf("ReadCodexUsagePage: %v", err)
		}
		usages = append(usages, page.Usages...)
		pages = append(pages, page)
		if !page.More {
			return usages, pages
		}
		resume = &page.Final
	}
	t.Fatal("drain did not terminate within 64 passes")
	return nil, nil
}

// codexPageUsageIDs returns each usage's MessageID in order.
func codexPageUsageIDs(usages []TailUsage) []string {
	ids := make([]string, len(usages))
	for i, u := range usages {
		ids[i] = u.MessageID
	}
	return ids
}

// TestReadCodexUsagePageDrainsBacklogAcrossBudgetPasses pins ga-amrzyf case 2's
// reader half: a backlog bigger than both the old 64 KiB tail window and the
// per-pass fact budget drains completely across bounded forward passes, each
// eligible token_count identity appearing exactly once, and no pass ever
// committing an offset into an unread line (a resume from every emitted
// checkpoint continues exactly at the next record).
func TestReadCodexUsagePageDrainsBacklogAcrossBudgetPasses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "2026", "11", "02", "rollout-pages-drain.jsonl")
	lines := []string{
		`{"timestamp":"2026-11-02T00:00:00Z","type":"turn_context","payload":{"model":"gpt-5.5"}}`,
		codexTokenCountLine("2026-11-02T00:00:01Z", 150, 100, 0, 50, 0),
	}
	for i := range 32 { // ~128 KiB: twice the old fixed tail window
		lines = append(lines, codexPageFillerLine(fmt.Sprintf("2026-11-02T00:00:02.%03dZ", i)))
	}
	lines = append(lines,
		codexTokenCountLine("2026-11-02T00:00:03Z", 300, 200, 0, 100, 0),
		codexTokenCountLine("2026-11-02T00:00:04Z", 450, 250, 0, 200, 0),
	)
	writeCodexUsageLines(t, path, lines)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// Fact budget 2 forces at least two passes; the byte budget stays at the
	// default so the filler exercises only the fact cap here.
	usages, pages := drainCodexUsagePages(t, path, nil, CodexUsagePageBudgets{Facts: 2})
	if got := strings.Join(codexPageUsageIDs(usages), ","); got != "total:150,total:300,total:450" {
		t.Fatalf("drained identities = %s, want each eligible invocation exactly once in file order", got)
	}
	if len(pages) < 2 {
		t.Fatalf("budgeted drain took %d pages, want >= 2 (the fact budget must bound each pass)", len(pages))
	}
	var prevOffset int64
	for i, page := range pages {
		if page.Incomplete != "" {
			t.Fatalf("page %d reported incomplete (%q) on a healthy source", i, page.Incomplete)
		}
		if page.Final.Offset <= prevOffset && i > 0 {
			t.Fatalf("page %d offset %d did not advance past %d", i, page.Final.Offset, prevOffset)
		}
		if len(page.Checkpoints) != len(page.Usages) {
			t.Fatalf("page %d has %d usages but %d checkpoints, want one continuation per usage", i, len(page.Usages), len(page.Checkpoints))
		}
		prevOffset = page.Final.Offset
	}
	if prevOffset != info.Size() {
		t.Fatalf("final offset %d != source size %d (the torn-free tail must be committed)", prevOffset, info.Size())
	}
	if pages[len(pages)-1].More {
		t.Fatal("the last page still reports More on a fully drained source")
	}
}

// TestReadCodexUsagePageResumesFromExistingCheckpoint pins the resume half of
// AC-3: a pass resumed from a committed checkpoint returns only the identities
// after it, carries the turn model announced before the checkpoint (so a model
// context that scrolled past any window still labels later invocations — case
// 3's mechanism), and skips a duplicate re-emission of the checkpointed last
// invocation instead of counting it twice.
func TestReadCodexUsagePageResumesFromExistingCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "2026", "11", "02", "rollout-pages-resume.jsonl")
	lines := []string{
		`{"timestamp":"2026-11-02T00:00:00Z","type":"turn_context","payload":{"model":"gpt-5.5"}}`,
		codexTokenCountLine("2026-11-02T00:00:01Z", 150, 100, 0, 50, 0),
	}
	for i := range 32 {
		lines = append(lines, codexPageFillerLine(fmt.Sprintf("2026-11-02T00:00:02.%03dZ", i)))
	}
	lines = append(lines,
		`{"timestamp":"2026-11-02T00:00:03Z","type":"turn_context","payload":{"model":"gpt-5.6"}}`,
		// The CLI re-emits the prior turn's final cumulative snapshot — same
		// total, same per-call usage — under the new turn_context; the resume
		// must not count it again.
		codexTokenCountLine("2026-11-02T00:00:04Z", 150, 100, 0, 50, 0),
		codexTokenCountLine("2026-11-02T00:00:05Z", 300, 200, 0, 100, 0),
	)
	writeCodexUsageLines(t, path, lines)

	// Pass 1: one fact only, stopping right after the first invocation.
	first, err := ReadCodexUsagePage(context.Background(), path, nil, CodexUsagePageBudgets{Facts: 1})
	if err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if len(first.Usages) != 1 || first.Usages[0].MessageID != "total:150" || first.Usages[0].Model != "gpt-5.5" {
		t.Fatalf("pass 1 usages = %+v, want the single gpt-5.5 invocation total:150", first.Usages)
	}

	// Pass 2 resumes from pass 1's final checkpoint, beyond the model switch.
	second, err := ReadCodexUsagePage(context.Background(), path, &first.Final, CodexUsagePageBudgets{Facts: 8})
	if err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if got := strings.Join(codexPageUsageIDs(second.Usages), ","); got != "total:300" {
		t.Fatalf("resumed identities = %s, want only total:300 (total:150 neither re-emitted nor double-counted by its re-emission)", got)
	}
	if u := second.Usages[0]; u.Model != "gpt-5.6" {
		t.Fatalf("resumed invocation model = %q, want gpt-5.6 (the turn_context before the resume point applies)", u.Model)
	}
	if second.Final.Offset <= first.Final.Offset {
		t.Fatal("the resumed pass must advance the committed offset")
	}
}

// TestReadCodexUsagePageToolOnlyPagesAdvance pins that a pass holding only
// non-usage records (tool outputs, turn_contexts) still commits its offset, so
// pure-filler prefixes are never rescanned and never stall the drain.
func TestReadCodexUsagePageToolOnlyPagesAdvance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "2026", "11", "02", "rollout-pages-toolonly.jsonl")
	var lines []string
	for i := range 8 {
		lines = append(lines, codexPageFillerLine(fmt.Sprintf("2026-11-02T00:00:%02dZ", i)))
	}
	lines = append(lines, codexTokenCountLine("2026-11-02T00:00:20Z", 150, 100, 0, 50, 0))
	writeCodexUsageLines(t, path, lines)

	// Byte budget sized to stop the first pass inside the filler prefix,
	// before any usage: a tool-only page.
	first, err := ReadCodexUsagePage(context.Background(), path, nil, CodexUsagePageBudgets{InputBytes: 12 * 1024})
	if err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if len(first.Usages) != 0 {
		t.Fatalf("pass 1 usages = %d, want 0 (a tool-only page)", len(first.Usages))
	}
	if !first.More || first.Final.Offset == 0 {
		t.Fatalf("a tool-only page must still commit progress: More=%t offset=%d", first.More, first.Final.Offset)
	}
	rest, err := ReadCodexUsagePage(context.Background(), path, &first.Final, CodexUsagePageBudgets{})
	if err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if got := strings.Join(codexPageUsageIDs(rest.Usages), ","); got != "total:150" {
		t.Fatalf("resumed identities = %s, want total:150 after the filler prefix", got)
	}
}

// TestReadCodexUsagePageCollapsesDuplicateEmissionsWithinAPage keeps the
// extractor's collapse contract on the forward reader: a duplicate cumulative
// total keeps the first-observed model and invocation time, refreshes the
// entry identity to the last emission, and still counts the invocation once.
func TestReadCodexUsagePageCollapsesDuplicateEmissionsWithinAPage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "2026", "11", "02", "rollout-pages-collapse.jsonl")
	writeCodexUsageLines(t, path, []string{
		`{"timestamp":"2026-11-02T00:00:00Z","type":"turn_context","payload":{"model":"gpt-5.5"}}`,
		codexTokenCountLine("2026-11-02T00:00:01Z", 150, 100, 0, 50, 0),
		`{"timestamp":"2026-11-02T00:00:02Z","type":"turn_context","payload":{"model":"gpt-5.6"}}`,
		// Same cumulative total and per-call usage: a re-emission of the
		// invocation above under the new turn_context.
		codexTokenCountLine("2026-11-02T00:00:03Z", 150, 100, 0, 50, 0),
		codexTokenCountLine("2026-11-02T00:00:04Z", 300, 200, 0, 100, 0),
	})

	page, err := ReadCodexUsagePage(context.Background(), path, nil, CodexUsagePageBudgets{})
	if err != nil {
		t.Fatalf("ReadCodexUsagePage: %v", err)
	}
	if got := strings.Join(codexPageUsageIDs(page.Usages), ","); got != "total:150,total:300" {
		t.Fatalf("identities = %s, want the re-emission collapsed to one entry", got)
	}
	if u := page.Usages[0]; u.Model != "gpt-5.5" {
		t.Fatalf("collapsed model = %q, want the first-observed gpt-5.5 (a switch must not relabel it)", u.Model)
	}
	if u := page.Usages[0]; u.Timestamp.UTC().Format(time.RFC3339) != "2026-11-02T00:00:01Z" {
		t.Fatalf("collapsed timestamp = %v, want the first emission's 00:00:01Z", u.Timestamp)
	}
	if u := page.Usages[0]; u.EntryUUID != "2026-11-02T00:00:03Z" {
		t.Fatalf("collapsed EntryUUID = %q, want the last emission's 00:00:03Z", u.EntryUUID)
	}
	if u := page.Usages[1]; u.Model != "gpt-5.6" {
		t.Fatalf("post-switch model = %q, want gpt-5.6", u.Model)
	}
}

// TestCodexUsageCheckpointRoundTrip pins the checkpoint's persisted form: a
// marshal/parse cycle reproduces every field, a foreign version is refused,
// and garbage is an error rather than a zero checkpoint that would silently
// restart progress.
func TestCodexUsageCheckpointRoundTrip(t *testing.T) {
	cp := CodexUsageCheckpoint{
		Version:             codexUsageCheckpointVersion,
		SourcePath:          "/codex/2026/11/18/rollout-x.jsonl",
		SourceID:            "019e7777-eeee-7000-8000-00000000000f",
		Offset:              12345,
		TurnModel:           "gpt-5.5",
		LastInvocationID:    "total:450",
		LastInvocationModel: "gpt-5.5",
		LastInvocationAt:    time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC),
	}
	raw, err := MarshalCodexUsageCheckpoint(cp)
	if err != nil {
		t.Fatalf("MarshalCodexUsageCheckpoint: %v", err)
	}
	back, err := ParseCodexUsageCheckpoint(raw)
	if err != nil {
		t.Fatalf("ParseCodexUsageCheckpoint: %v", err)
	}
	if back != cp {
		t.Fatalf("round trip = %+v, want %+v", back, cp)
	}

	foreign := strings.Replace(raw, fmt.Sprintf(`"version":%d`, codexUsageCheckpointVersion), `"version":99`, 1)
	if _, err := ParseCodexUsageCheckpoint(foreign); err == nil {
		t.Fatal("a foreign checkpoint version must be refused, not parsed")
	}
	if _, err := ParseCodexUsageCheckpoint("not json"); err == nil {
		t.Fatal("garbage must be an error, not a zero checkpoint")
	}
}

// TestReadCodexUsagePageSpansRestartWithDistantModelContext pins ga-amrzyf
// case 3's mechanism on the page reader: a turn_context more than 2 MiB before
// its invocations (far past the old 1 MiB model-seed budget) still labels them
// when pages thread a checkpoint across a restart, and a later switch plus
// re-emission keeps the original invocation's model and time.
func TestReadCodexUsagePageSpansRestartWithDistantModelContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-distant-model.jsonl")
	var lines []string
	lines = append(lines,
		`{"timestamp":"2026-11-18T09:00:00Z","type":"turn_context","payload":{"model":"gpt-5.5"}}`,
		codexTokenCountLine("2026-11-18T09:00:01Z", 150, 100, 0, 50, 0),
	)
	// More than 2 MiB of tool output between the announcement and the next
	// invocation: past the old seed scan, and past several 512 KiB pages.
	for i := range 520 {
		lines = append(lines, codexPageFillerLine(time.Date(2026, 11, 18, 9, 0, 2, i*1000000, time.UTC).Format(time.RFC3339Nano)))
	}
	lines = append(lines,
		codexTokenCountLine("2026-11-18T10:00:00Z", 300, 200, 0, 100, 0),
		`{"timestamp":"2026-11-18T10:00:01Z","type":"turn_context","payload":{"model":"gpt-5.6"}}`,
		codexTokenCountLine("2026-11-18T10:00:02Z", 300, 200, 0, 100, 0), // re-emission
		codexTokenCountLine("2026-11-18T10:00:03Z", 450, 250, 0, 200, 0),
	)
	writeCodexUsageLines(t, path, lines)

	// Drain in 512 KiB pages, round-tripping every continuation through the
	// persisted form between passes: each resume is a restart.
	var usages []TailUsage
	resume := (*CodexUsageCheckpoint)(nil)
	passes := 0
	for passes < 64 {
		if resume != nil {
			raw, err := MarshalCodexUsageCheckpoint(*resume)
			if err != nil {
				t.Fatalf("marshal continuation: %v", err)
			}
			parsed, err := ParseCodexUsageCheckpoint(raw)
			if err != nil {
				t.Fatalf("parse continuation: %v", err)
			}
			resume = &parsed
		}
		page, err := ReadCodexUsagePage(context.Background(), path, resume, CodexUsagePageBudgets{InputBytes: 512 * 1024})
		if err != nil {
			t.Fatalf("pass %d: %v", passes, err)
		}
		usages = append(usages, page.Usages...)
		passes++
		if !page.More {
			break
		}
		resume = &page.Final
	}
	if passes < 3 {
		t.Fatalf("drain took %d passes, want the 2 MiB gap to span several 512 KiB pages", passes)
	}
	if got := strings.Join(codexPageUsageIDs(usages), ","); got != "total:150,total:300,total:450" {
		t.Fatalf("identities = %s, want three invocations once each (re-emission collapsed or skipped)", got)
	}
	if u := usages[1]; u.Model != "gpt-5.5" || u.Timestamp.UTC().Format(time.RFC3339) != "2026-11-18T10:00:00Z" {
		t.Fatalf("distant-context invocation = model %q at %v, want gpt-5.5 at 10:00:00Z (the >2 MiB-back announcement labels it)", u.Model, u.Timestamp)
	}
	if u := usages[2]; u.Model != "gpt-5.6" {
		t.Fatalf("post-switch invocation model = %q, want gpt-5.6", u.Model)
	}
}
