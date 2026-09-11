package events

import (
	"testing"
	"time"
)

func TestArchiveFilenameRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		ts       time.Time
		first    uint64
		last     uint64
		wantBase string
	}{
		{
			name:     "midnight UTC",
			ts:       time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC),
			first:    1,
			last:     100,
			wantBase: "events.jsonl.archive-20260507T000000Z-seq-1-100.gz",
		},
		{
			name:     "afternoon UTC",
			ts:       time.Date(2026, 5, 7, 18, 30, 45, 0, time.UTC),
			first:    1234,
			last:     5678,
			wantBase: "events.jsonl.archive-20260507T183045Z-seq-1234-5678.gz",
		},
		{
			name:     "non-UTC zone is normalized to UTC",
			ts:       time.Date(2026, 5, 7, 14, 0, 0, 0, time.FixedZone("EST", -5*3600)),
			first:    1,
			last:     2,
			wantBase: "events.jsonl.archive-20260507T190000Z-seq-1-2.gz",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatArchiveBasename(tc.ts, tc.first, tc.last)
			if got != tc.wantBase {
				t.Errorf("formatArchiveBasename(%v,%d,%d) = %q, want %q",
					tc.ts, tc.first, tc.last, got, tc.wantBase)
			}
			info, err := parseArchiveBasename(tc.wantBase)
			if err != nil {
				t.Fatalf("parseArchiveBasename(%q): %v", tc.wantBase, err)
			}
			if !info.Timestamp.Equal(tc.ts.UTC()) {
				t.Errorf("Timestamp = %v, want %v", info.Timestamp, tc.ts.UTC())
			}
			if info.FirstSeq != tc.first {
				t.Errorf("FirstSeq = %d, want %d", info.FirstSeq, tc.first)
			}
			if info.LastSeq != tc.last {
				t.Errorf("LastSeq = %d, want %d", info.LastSeq, tc.last)
			}
		})
	}
}

func TestParseArchiveBasenameRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"not an archive", "events.jsonl"},
		{"legacy archive (no seq)", "events.jsonl.archive-20260416.gz"},
		{"missing .gz", "events.jsonl.archive-20260507T000000Z-seq-1-100"},
		{"non-numeric first", "events.jsonl.archive-20260507T000000Z-seq-foo-100.gz"},
		{"non-numeric last", "events.jsonl.archive-20260507T000000Z-seq-1-bar.gz"},
		{"missing seq segment", "events.jsonl.archive-20260507T000000Z.gz"},
		{"unrelated file", "snapshot-20260507.tar.gz"},
		{"first > last", "events.jsonl.archive-20260507T000000Z-seq-200-100.gz"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseArchiveBasename(tc.in); err == nil {
				t.Errorf("parseArchiveBasename(%q): expected error, got nil", tc.in)
			}
		})
	}
}

func TestIsLegacyArchive(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"events.jsonl.archive-20260416.gz", true},
		{"events.jsonl.archive-20260507T000000Z-seq-1-100.gz", false},
		{"events.jsonl", false},
		{"events.jsonl.archive-20260416.tar.gz", false},
		{"events.jsonl.archive-bogus.gz", false},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got := isLegacyArchiveBasename(tc.in)
			if got != tc.want {
				t.Errorf("isLegacyArchiveBasename(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestArchiveOverlapsFilter(t *testing.T) {
	info := archiveInfo{
		Basename:  "events.jsonl.archive-20260507T000000Z-seq-100-200.gz",
		Timestamp: time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC),
		FirstSeq:  100,
		LastSeq:   200,
	}
	tests := []struct {
		name string
		f    Filter
		want bool
	}{
		{"empty filter overlaps everything", Filter{}, true},
		{"AfterSeq below archive range", Filter{AfterSeq: 50}, true},
		{"AfterSeq inside archive range", Filter{AfterSeq: 150}, true},
		{"AfterSeq at archive last seq", Filter{AfterSeq: 200}, false},
		{"AfterSeq above archive range", Filter{AfterSeq: 250}, false},
		{"BeforeSeq above archive range", Filter{BeforeSeq: 250}, true},
		{"BeforeSeq inside archive range", Filter{BeforeSeq: 150}, true},
		{"BeforeSeq just above archive first seq", Filter{BeforeSeq: 101}, true},
		{"BeforeSeq at archive first seq", Filter{BeforeSeq: 100}, false},
		{"BeforeSeq below archive range", Filter{BeforeSeq: 50}, false},
		{"AfterSeq and BeforeSeq window inside range", Filter{AfterSeq: 120, BeforeSeq: 180}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := archiveOverlapsFilter(info, tc.f)
			if got != tc.want {
				t.Errorf("overlap = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestArchiveOverlapsFilterSkipsArchivesOlderThanSince pins the skip-fast
// contract for time-bounded reads: an archive whose rotation timestamp
// predates filter.Since cannot contain a matching event, so the reader must
// not gunzip it. The archive filename records only info.Timestamp, the
// rotation instant TRUNCATED to whole seconds — the true rotation instant T
// can land anywhere in [info.Timestamp, info.Timestamp+1s). Every event in
// the archive was appended before T, so event.Time <= T, which only gives
// event.Time < info.Timestamp+1s (see #4628). A Since strictly inside that
// truncation second must therefore still be read; only a Since at or beyond
// info.Timestamp+1s can be safely skipped.
func TestArchiveOverlapsFilterSkipsArchivesOlderThanSince(t *testing.T) {
	// Rotated 2026-05-07; the live fleet queries with ?since=5m.
	info := archiveInfo{
		Basename:  "events.jsonl.archive-20260507T000000Z-seq-100-200.gz",
		Timestamp: time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC),
		FirstSeq:  100,
		LastSeq:   200,
	}
	tests := []struct {
		name string
		f    Filter
		want bool
	}{
		{
			name: "Since well after archive rotation is skippable",
			f:    Filter{Since: time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)},
			want: false,
		},
		{
			name: "Since one second after archive rotation must still be read (true rotation instant is unknown within the truncation second)",
			f:    Filter{Since: time.Date(2026, 5, 7, 0, 0, 1, 0, time.UTC)},
			want: true,
		},
		{
			name: "Since one second and one nanosecond after archive rotation is skippable",
			f:    Filter{Since: time.Date(2026, 5, 7, 0, 0, 1, 1, time.UTC)},
			want: false,
		},
		{
			name: "Since strictly inside the rotation's truncation second must still be read",
			f:    Filter{Since: time.Date(2026, 5, 7, 0, 0, 0, 500000000, time.UTC)},
			want: true,
		},
		{
			name: "Since before archive rotation must still be read",
			f:    Filter{Since: time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC)},
			want: true,
		},
		{
			name: "Since exactly at rotation must still be read (inclusive bound)",
			f:    Filter{Since: time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)},
			want: true,
		},
		{
			name: "zero Since is unbounded and must still be read",
			f:    Filter{},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := archiveOverlapsFilter(info, tc.f); got != tc.want {
				t.Errorf("archiveOverlapsFilter(Since=%v) = %v, want %v",
					tc.f.Since, got, tc.want)
			}
		})
	}
}
