package sessionlog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// codexPagesRollout writes lines as a rollout under a fresh root and returns
// the root and the rollout path.
func codexPagesRollout(t *testing.T, lines []string) (string, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "2026", "04", "16", "rollout-2026-04-16T21-49-29-pages.jsonl")
	writeCodexUsageLines(t, path, lines)
	return root, path
}

func appendCodexUsageLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %q for append: %v", path, err)
	}
	defer f.Close() //nolint:errcheck // test file
	for _, line := range lines {
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatalf("append to %q: %v", path, err)
		}
	}
}

// drainCodexUsagePages reads pages from cp until a pass leaves nothing more,
// returning every emitted message id in order, the pages read and the final
// checkpoint.
func drainCodexUsagePages(t *testing.T, root, path string, cp CodexUsageCheckpoint, limits CodexUsagePageLimits) ([]string, []CodexUsagePage, CodexUsageCheckpoint) {
	t.Helper()
	var ids []string
	var pages []CodexUsagePage
	for pass := 0; ; pass++ {
		if pass > 1000 {
			t.Fatal("codex usage pages never drained")
		}
		page, err := ReadCodexUsagePage(context.Background(), CodexUsagePageRequest{
			SearchPaths: []string{root},
			Path:        path,
			From:        cp,
			Limits:      limits,
		})
		if err != nil {
			t.Fatalf("ReadCodexUsagePage pass %d: %v", pass, err)
		}
		pages = append(pages, page)
		for _, e := range page.Entries {
			ids = append(ids, e.Usage.MessageID)
		}
		cp = page.Checkpoint
		if !page.More {
			return ids, pages, cp
		}
	}
}

func TestReadCodexUsagePageDrainsBacklogBeyondTailWindow(t *testing.T) {
	lines := []string{
		codexSessionMetaLine("2026-04-16T21:49:30.734Z", "/work/dir"),
		codexTurnContextLine("2026-04-16T21:49:30.901Z", "gpt-5.5"),
		codexTokenCountLine("2026-04-16T21:49:38.304Z", 150, 100, 0, 50, 0),
		codexTokenCountLine("2026-04-16T21:49:39.304Z", 300, 100, 0, 50, 0),
	}
	// Tool output far past the 64 KiB tail window the old extractor read.
	lines = append(lines, codexFillerLines(t, 128*1024)...)
	lines = append(lines,
		codexTokenCountLine("2026-04-16T21:50:38.304Z", 450, 100, 0, 50, 0),
		codexTokenCountLine("2026-04-16T21:50:39.304Z", 600, 100, 0, 50, 0),
	)
	root, path := codexPagesRollout(t, lines)

	ids, pages, final := drainCodexUsagePages(t, root, path, CodexUsageCheckpoint{}, CodexUsagePageLimits{MaxBytes: 16 * 1024})

	want := []string{"total:150", "total:300", "total:450", "total:600"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("drained ids = %v, want %v", ids, want)
	}
	if len(pages) < 3 {
		t.Fatalf("drained in %d pages, want the 16 KiB budget to need several", len(pages))
	}
	sawToolOnly := false
	for i, page := range pages[:len(pages)-1] {
		if !page.More {
			t.Fatalf("page %d stopped short without More", i)
		}
		if len(page.Issues) != 0 {
			t.Fatalf("page %d issues = %v, want none", i, page.Issues)
		}
		if page.BytesRead <= 0 {
			t.Fatalf("page %d read %d bytes, want progress", i, page.BytesRead)
		}
		if len(page.Entries) == 0 {
			sawToolOnly = true
		}
	}
	if !sawToolOnly {
		t.Fatal("no page was tool output only; the fixture must make the budget land inside the filler")
	}
	last := pages[len(pages)-1]
	if last.Incomplete() {
		t.Fatalf("final page incomplete: more=%v issues=%v", last.More, last.Issues)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if final.Offset != info.Size() {
		t.Fatalf("final offset = %d, want rollout size %d", final.Offset, info.Size())
	}
	for _, page := range pages {
		for _, e := range page.Entries {
			if e.Usage.Model != "gpt-5.5" {
				t.Fatalf("%s model = %q, want gpt-5.5", e.Usage.MessageID, e.Usage.Model)
			}
		}
	}

	again, err := ReadCodexUsagePage(context.Background(), CodexUsagePageRequest{SearchPaths: []string{root}, Path: path, From: final})
	if err != nil {
		t.Fatalf("re-read at end: %v", err)
	}
	if len(again.Entries) != 0 || again.Incomplete() {
		t.Fatalf("re-read at end = %d entries, more=%v issues=%v; want nothing", len(again.Entries), again.More, again.Issues)
	}
}

func TestReadCodexUsagePageFactBudgetResumesAfterEachEntry(t *testing.T) {
	root, path := codexPagesRollout(t, []string{
		codexSessionMetaLine("2026-04-16T21:49:30.734Z", "/work/dir"),
		codexTurnContextLine("2026-04-16T21:49:30.901Z", "gpt-5.5"),
		codexTokenCountLine("2026-04-16T21:49:38.304Z", 150, 100, 0, 50, 0),
		codexTokenCountLine("2026-04-16T21:49:39.304Z", 300, 100, 0, 50, 0),
		codexTokenCountLine("2026-04-16T21:49:40.304Z", 450, 100, 0, 50, 0),
	})

	ids, pages, _ := drainCodexUsagePages(t, root, path, CodexUsageCheckpoint{}, CodexUsagePageLimits{MaxFacts: 1})
	if want := []string{"total:150", "total:300", "total:450"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i, page := range pages {
		if len(page.Entries) > 1 {
			t.Fatalf("page %d yielded %d entries, want at most 1", i, len(page.Entries))
		}
		if len(page.Entries) == 1 && page.Entries[0].Checkpoint != page.Checkpoint {
			t.Fatalf("page %d: entry checkpoint %+v != page checkpoint %+v", i, page.Entries[0].Checkpoint, page.Checkpoint)
		}
	}

	// Resuming from the first entry's own checkpoint yields only later facts.
	first := pages[0].Entries[0].Checkpoint
	rest, _, _ := drainCodexUsagePages(t, root, path, first, CodexUsagePageLimits{})
	if want := []string{"total:300", "total:450"}; !reflect.DeepEqual(rest, want) {
		t.Fatalf("resume from first entry = %v, want %v", rest, want)
	}
}

func TestReadCodexUsagePageResumesAppendedRecords(t *testing.T) {
	root, path := codexPagesRollout(t, []string{
		codexSessionMetaLine("2026-04-16T21:49:30.734Z", "/work/dir"),
		codexTurnContextLine("2026-04-16T21:49:30.901Z", "gpt-5.5"),
		codexTokenCountLine("2026-04-16T21:49:38.304Z", 150, 100, 0, 50, 0),
	})
	ids, _, cp := drainCodexUsagePages(t, root, path, CodexUsageCheckpoint{}, CodexUsagePageLimits{})
	if want := []string{"total:150"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("first drain = %v, want %v", ids, want)
	}
	if cp.SourceID != "019d9845-4273-7ee3-a7d7-15b71ec6f096" {
		t.Fatalf("checkpoint source id = %q, want the session_meta id", cp.SourceID)
	}

	appendCodexUsageLines(t, path, codexTokenCountLine("2026-04-16T21:49:39.304Z", 300, 100, 0, 50, 0))
	ids, _, _ = drainCodexUsagePages(t, root, path, cp, CodexUsagePageLimits{})
	if want := []string{"total:300"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("after append = %v, want %v", ids, want)
	}
}

func TestReadCodexUsagePageRejectsPathOutsideSearchRoots(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "rollout-out.jsonl")
	writeCodexUsageLines(t, outside, []string{codexSessionMetaLine("2026-04-16T21:49:30.734Z", "/work/dir")})
	if _, err := ReadCodexUsagePage(context.Background(), CodexUsagePageRequest{SearchPaths: []string{t.TempDir()}, Path: outside}); err == nil {
		t.Fatal("a rollout outside every codex root must be rejected")
	}
}

func TestCodexUsageCheckpointRoundTrip(t *testing.T) {
	root, path := codexPagesRollout(t, []string{
		codexSessionMetaLine("2026-04-16T21:49:30.734Z", "/work/dir"),
		codexTurnContextLine("2026-04-16T21:49:30.901Z", "gpt-5.5"),
		codexTokenCountLine("2026-04-16T21:49:38.304Z", 150, 100, 0, 50, 0),
	})
	_, _, cp := drainCodexUsagePages(t, root, path, CodexUsageCheckpoint{}, CodexUsagePageLimits{})
	cp.Cursor = "total:150"

	raw, err := MarshalCodexUsageCheckpoint(cp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := ParseCodexUsageCheckpoint(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !got.LastAt.Equal(cp.LastAt) || !got.EmitFloor.Equal(cp.EmitFloor) {
		t.Fatalf("times did not round-trip: got %+v want %+v", got, cp)
	}
	got.LastAt = cp.LastAt
	got.EmitFloor = cp.EmitFloor
	if got != cp {
		t.Fatalf("round trip = %+v, want %+v", got, cp)
	}

	if zero, err := ParseCodexUsageCheckpoint("  "); err != nil || !zero.IsZero() {
		t.Fatalf("empty checkpoint = %+v, %v; want zero, nil", zero, err)
	}
	if _, err := ParseCodexUsageCheckpoint(`{"v":99,"path":"/x"}`); !errors.Is(err, ErrCodexUsageCheckpointVersion) {
		t.Fatalf("other version err = %v, want ErrCodexUsageCheckpointVersion", err)
	}
	if _, err := ParseCodexUsageCheckpoint(`{not json`); err == nil {
		t.Fatal("undecodable checkpoint must be an error")
	}
}

// TestReadCodexUsagePageKeepsModelContextAcrossPagesAndRestart reads a rollout
// whose model context lies more than 2 MiB before an invocation, followed by a
// model switch and the re-emitted snapshot of that invocation, in bounded
// pages whose checkpoint is stored and parsed between each (a restart).
func TestReadCodexUsagePageKeepsModelContextAcrossPagesAndRestart(t *testing.T) {
	lines := []string{
		codexSessionMetaLine("2026-04-16T21:49:30.734Z", "/work/dir"),
		codexTurnContextLine("2026-04-16T21:49:30.901Z", "gpt-5.5"),
		codexTokenCountLine("2026-04-16T21:49:38.304Z", 150, 100, 0, 50, 0),
	}
	lines = append(lines, codexFillerLines(t, 2*1024*1024+512*1024)...)
	lines = append(lines,
		codexTokenCountLine("2026-04-16T21:50:38.304Z", 300, 100, 0, 50, 0),
		codexTurnContextLine("2026-04-16T21:50:40.000Z", "gpt-5.6"),
		codexTokenCountLine("2026-04-16T21:50:40.100Z", 300, 100, 0, 50, 0), // re-emission
		codexTokenCountLine("2026-04-16T21:50:48.304Z", 450, 100, 0, 50, 0),
	)
	root, path := codexPagesRollout(t, lines)

	type got struct{ id, model, at string }
	var facts []got
	var cp CodexUsageCheckpoint
	pages := 0
	for more := true; more; pages++ {
		if pages > 100 {
			t.Fatal("pages never drained")
		}
		page, err := ReadCodexUsagePage(context.Background(), CodexUsagePageRequest{
			SearchPaths: []string{root}, Path: path, From: cp,
			Limits: CodexUsagePageLimits{MaxBytes: 256 * 1024},
		})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		for _, e := range page.Entries {
			facts = append(facts, got{e.Usage.MessageID, e.Usage.Model, e.Usage.Timestamp.UTC().Format("15:04:05.000")})
		}
		raw, err := MarshalCodexUsageCheckpoint(page.Checkpoint)
		if err != nil {
			t.Fatalf("marshal page %d checkpoint: %v", pages, err)
		}
		if cp, err = ParseCodexUsageCheckpoint(raw); err != nil {
			t.Fatalf("parse page %d checkpoint: %v", pages, err)
		}
		more = page.More
	}
	if pages < 3 {
		t.Fatalf("read in %d pages, want the 256 KiB budget to split the 2 MiB gap", pages)
	}
	want := []got{
		{"total:150", "gpt-5.5", "21:49:38.304"},
		{"total:300", "gpt-5.5", "21:50:38.304"},
		{"total:450", "gpt-5.6", "21:50:48.304"},
	}
	if !reflect.DeepEqual(facts, want) {
		t.Fatalf("facts = %+v, want %+v", facts, want)
	}
}

func appendCodexUsageRaw(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %q for append: %v", path, err)
	}
	defer f.Close() //nolint:errcheck // test file
	if _, err := f.WriteString(s); err != nil {
		t.Fatalf("append to %q: %v", path, err)
	}
}

// TestReadCodexUsagePageReportsIncompleteReads pins what a pass reports for
// each way it cannot read its rollout completely: the issue, whether the
// invocations around it are emitted, and where the checkpoint resumes.
func TestReadCodexUsagePageReportsIncompleteReads(t *testing.T) {
	head := []string{
		codexSessionMetaLine("2026-04-16T21:49:30.734Z", "/work/dir"),
		codexTurnContextLine("2026-04-16T21:49:30.901Z", "gpt-5.5"),
		codexTokenCountLine("2026-04-16T21:49:38.304Z", 150, 100, 0, 50, 0),
	}
	headBytes := int64(len(strings.Join(head, "\n")) + 1)
	tc300 := codexTokenCountLine("2026-04-16T21:49:39.304Z", 300, 100, 0, 50, 0)
	tc450 := codexTokenCountLine("2026-04-16T21:49:40.304Z", 450, 100, 0, 50, 0)
	read := func(t *testing.T, root, path string, from CodexUsageCheckpoint, limits CodexUsagePageLimits) CodexUsagePage {
		t.Helper()
		page, err := ReadCodexUsagePage(context.Background(), CodexUsagePageRequest{SearchPaths: []string{root}, Path: path, From: from, Limits: limits})
		if err != nil {
			t.Fatalf("ReadCodexUsagePage: %v", err)
		}
		return page
	}
	ids := func(page CodexUsagePage) []string {
		var out []string
		for _, e := range page.Entries {
			out = append(out, e.Usage.MessageID)
		}
		return out
	}
	expect := func(t *testing.T, page CodexUsagePage, wantIDs []string, wantIssue string) {
		t.Helper()
		if got := ids(page); !reflect.DeepEqual(got, wantIDs) {
			t.Fatalf("ids = %v, want %v", got, wantIDs)
		}
		if !reflect.DeepEqual(page.Issues, []string{wantIssue}) {
			t.Fatalf("issues = %v, want [%s]", page.Issues, wantIssue)
		}
		if !page.Incomplete() {
			t.Fatal("a page with an issue must be incomplete")
		}
	}

	t.Run("torn tail stays pending", func(t *testing.T) {
		root, path := codexPagesRollout(t, head)
		appendCodexUsageRaw(t, path, tc300)
		page := read(t, root, path, CodexUsageCheckpoint{}, CodexUsagePageLimits{})
		expect(t, page, []string{"total:150"}, CodexUsageIssueTornTail)
		if page.Checkpoint.Offset != headBytes {
			t.Fatalf("offset = %d, want %d: the torn record must stay unread", page.Checkpoint.Offset, headBytes)
		}
		appendCodexUsageRaw(t, path, "\n")
		next := read(t, root, path, page.Checkpoint, CodexUsagePageLimits{})
		if got := ids(next); !reflect.DeepEqual(got, []string{"total:300"}) || next.Incomplete() {
			t.Fatalf("after the record completes = %v, more=%v issues=%v; want [total:300], complete", got, next.More, next.Issues)
		}
	})

	t.Run("oversized usage record is skipped and reported", func(t *testing.T) {
		big := strings.TrimSuffix(tc300, "}") + `,"zz_pad":"` + strings.Repeat("p", 4096) + `"}`
		root, path := codexPagesRollout(t, append(append(append([]string(nil), head...), big), tc450))
		limits := CodexUsagePageLimits{MaxRecordBytes: 2048}
		page := read(t, root, path, CodexUsageCheckpoint{}, limits)
		expect(t, page, []string{"total:150", "total:450"}, CodexUsageIssueOversizedRecord)
		next := read(t, root, path, page.Checkpoint, limits)
		if len(next.Entries) != 0 || next.Incomplete() {
			t.Fatalf("re-read past the oversized record = %v, more=%v issues=%v; want nothing", ids(next), next.More, next.Issues)
		}
	})

	t.Run("oversized record past the read window is discarded over passes", func(t *testing.T) {
		big := strings.TrimSuffix(tc300, "}") + `,"zz_pad":"` + strings.Repeat("p", 8192) + `"}`
		root, path := codexPagesRollout(t, append(append(append([]string(nil), head...), big), tc450))
		limits := CodexUsagePageLimits{MaxBytes: 1024, MaxRecordBytes: 2048}
		got, pages, _ := drainCodexUsagePages(t, root, path, CodexUsageCheckpoint{}, limits)
		if !reflect.DeepEqual(got, []string{"total:150", "total:450"}) {
			t.Fatalf("drained ids = %v, want [total:150 total:450]", got)
		}
		reported := false
		for _, p := range pages {
			for _, issue := range p.Issues {
				reported = reported || issue == CodexUsageIssueOversizedRecord
			}
		}
		if !reported {
			t.Fatal("no pass reported the oversized usage record")
		}
	})

	t.Run("malformed record is reported", func(t *testing.T) {
		root, path := codexPagesRollout(t, append(append(append([]string(nil), head...), `{"type":"event_msg","payload":{"type":"token_count"`), tc450))
		page := read(t, root, path, CodexUsageCheckpoint{}, CodexUsagePageLimits{})
		expect(t, page, []string{"total:150", "total:450"}, CodexUsageIssueMalformedRecord)
	})

	t.Run("truncated source restarts from byte zero", func(t *testing.T) {
		root, path := codexPagesRollout(t, append(append([]string(nil), head...), tc300))
		first := read(t, root, path, CodexUsageCheckpoint{}, CodexUsagePageLimits{})
		writeCodexUsageLines(t, path, head)
		page := read(t, root, path, first.Checkpoint, CodexUsagePageLimits{})
		expect(t, page, []string{"total:150"}, CodexUsageIssueSourceTruncated)
		if page.Checkpoint.Offset != headBytes {
			t.Fatalf("offset = %d, want the replay to reach %d", page.Checkpoint.Offset, headBytes)
		}
	})

	t.Run("replaced source restarts from byte zero", func(t *testing.T) {
		root, path := codexPagesRollout(t, append(append([]string(nil), head...), tc300))
		first := read(t, root, path, CodexUsageCheckpoint{}, CodexUsagePageLimits{})
		replaced := append([]string(nil), head...)
		replaced[0] = strings.Replace(replaced[0], "019d9845-4273", "019d9845-9999", 1)
		writeCodexUsageLines(t, path, append(append(replaced, tc300), tc450))
		page := read(t, root, path, first.Checkpoint, CodexUsagePageLimits{})
		expect(t, page, []string{"total:150", "total:300", "total:450"}, CodexUsageIssueSourceReplaced)
		if page.Checkpoint.SourceID != "019d9845-9999-7ee3-a7d7-15b71ec6f096" {
			t.Fatalf("source id = %q, want the replacement's", page.Checkpoint.SourceID)
		}
	})
}
