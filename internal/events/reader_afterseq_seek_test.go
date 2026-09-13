package events

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSeqLog writes a monotonic append-only log of n events, seq 1..n, and
// returns its path. Every other event is type "b" so filters have something to
// discriminate on.
func writeSeqLog(t *testing.T, n int) string {
	t.Helper()
	return writeSeqLogFrom(t, 1, n)
}

// writeSeqLogFrom writes n events whose seq starts at first. A log whose lowest
// seq sits well above zero models the ordinary post-rotation active file, which
// a stale cursor can point below.
func writeSeqLogFrom(t *testing.T, first uint64, n int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var sb strings.Builder
	for i := 0; i < n; i++ {
		seq := first + uint64(i)
		typ := "a"
		if seq%2 == 0 {
			typ = "b"
		}
		fmt.Fprintf(&sb, `{"seq":%d,"type":%q,"ts":"2026-08-25T00:00:00Z","actor":"t","subject":"s%d"}`+"\n", seq, typ, seq)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	return path
}

// TestActiveScanStart pins the seq->offset boundary search on the active log.
// The append-only log has strictly increasing seq, so every line at or below
// AfterSeq can be skipped wholesale; activeScanStart must land exactly on the
// first line above the cursor and never before it (which would be slow) or
// after it (which would silently drop events).
func TestActiveScanStart(t *testing.T) {
	const n = 500
	path := writeSeqLog(t, n)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close() //nolint:errcheck // read-only file
	st, err := f.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	size := st.Size()

	// AfterSeq=0 means "no cursor": start at byte 0.
	if got := activeScanStart(f, size, 0); got != 0 {
		t.Fatalf("activeScanStart(afterSeq=0) = %d, want 0", got)
	}
	// A cursor below the log's first seq cannot skip anything: after a rotation
	// the active file starts high and a stale cursor sits below every line in it.
	rotated := writeSeqLogFrom(t, 5000, 100)
	f2, err := os.Open(rotated)
	if err != nil {
		t.Fatalf("open rotated: %v", err)
	}
	defer f2.Close() //nolint:errcheck // read-only file
	st2, err := f2.Stat()
	if err != nil {
		t.Fatalf("stat rotated: %v", err)
	}
	if got := activeScanStart(f2, st2.Size(), 100); got != 0 {
		t.Fatalf("activeScanStart(below first seq) = %d, want 0", got)
	}
	// A cursor at or beyond the last seq skips the whole file.
	if got := activeScanStart(f, size, n); got != size {
		t.Fatalf("activeScanStart(afterSeq=%d) = %d, want size %d", n, got, size)
	}
	if got := activeScanStart(f, size, n+1000); got != size {
		t.Fatalf("activeScanStart(beyond last) = %d, want size %d", n+1000, size)
	}
	// Interior cursors must land exactly on the start of the first line whose
	// seq exceeds the cursor. Read the line AT the offset directly: seqLineAt
	// treats a nonzero offset as mid-line and would report the following line.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	for _, cursor := range []uint64{1, 2, 7, 128, 499} {
		off := activeScanStart(f, size, cursor)
		if off <= 0 || off >= size {
			t.Fatalf("cursor=%d: offset %d out of range (size %d)", cursor, off, size)
		}
		if off > 0 && raw[off-1] != '\n' {
			t.Fatalf("cursor=%d: offset %d is not a line start", cursor, off)
		}
		line, _, _ := bytes.Cut(raw[off:], []byte("\n"))
		var probe struct {
			Seq uint64 `json:"seq"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			t.Fatalf("cursor=%d: line at %d not parseable: %v", cursor, off, err)
		}
		if probe.Seq != cursor+1 {
			t.Fatalf("cursor=%d: line at offset %d has seq %d, want %d", cursor, off, probe.Seq, cursor+1)
		}
	}
}

// TestReadFilteredAfterSeqEquivalence is the safety net for the optimization:
// for every cursor, the seek path must return exactly what a full scan returns.
func TestReadFilteredAfterSeqEquivalence(t *testing.T) {
	const n = 300
	path := writeSeqLog(t, n)

	all, err := ReadFiltered(path, Filter{})
	if err != nil {
		t.Fatalf("ReadFiltered(all): %v", err)
	}
	if len(all) != n {
		t.Fatalf("baseline read %d events, want %d", len(all), n)
	}

	for cursor := uint64(0); cursor <= n+1; cursor++ {
		for _, typ := range []string{"", "a", "b"} {
			got, err := ReadFiltered(path, Filter{AfterSeq: cursor, Type: typ})
			if err != nil {
				t.Fatalf("cursor=%d type=%q: %v", cursor, typ, err)
			}
			var want []Event
			for _, e := range all {
				if e.Seq > cursor && (typ == "" || e.Type == typ) {
					want = append(want, e)
				}
			}
			if len(got) != len(want) {
				t.Fatalf("cursor=%d type=%q: got %d events, want %d", cursor, typ, len(got), len(want))
			}
			for i := range got {
				if got[i].Seq != want[i].Seq {
					t.Fatalf("cursor=%d type=%q: event %d seq=%d, want %d", cursor, typ, i, got[i].Seq, want[i].Seq)
				}
			}
		}
	}
}

// TestReadFilteredAfterSeqMalformedLines pins that the seek path degrades to a
// correct result when the log contains unparseable lines, which the scanner has
// always tolerated by skipping.
func TestReadFilteredAfterSeqMalformedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	body := `{"seq":1,"type":"a"}
not json at all
{"seq":2,"type":"a"}
{"seq":3,"type":"a"}
{truncated
{"seq":4,"type":"a"}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	for cursor, wantSeqs := range map[uint64][]uint64{
		0: {1, 2, 3, 4},
		1: {2, 3, 4},
		2: {3, 4},
		3: {4},
		4: nil,
	} {
		got, err := ReadFiltered(path, Filter{AfterSeq: cursor})
		if err != nil {
			t.Fatalf("cursor=%d: %v", cursor, err)
		}
		if len(got) != len(wantSeqs) {
			t.Fatalf("cursor=%d: got %d events, want %d", cursor, len(got), len(wantSeqs))
		}
		for i, s := range wantSeqs {
			if got[i].Seq != s {
				t.Fatalf("cursor=%d: event %d seq=%d, want %d", cursor, i, got[i].Seq, s)
			}
		}
	}
}
