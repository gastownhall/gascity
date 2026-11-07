package session

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

type terminalHistoryRow struct {
	name      string
	template  string
	origin    string
	workerDir string
	workDir   string
	reason    string
	at        string
	closed    bool
}

func seedTerminalHistory(t *testing.T, store beads.Store, r terminalHistoryRow) {
	t.Helper()
	meta := map[string]string{
		"session_name":   r.name,
		"template":       r.template,
		"session_origin": r.origin,
	}
	for k, v := range map[string]string{
		"worker_dir":                 r.workerDir,
		"work_dir":                   r.workDir,
		"provider_terminal_error":    r.reason,
		"provider_terminal_error_at": r.at,
	} {
		if v != "" {
			meta[k] = v
		}
	}
	b, err := store.Create(beads.Bead{
		Title:    r.name,
		Type:     BeadType,
		Labels:   []string{LabelSession},
		Metadata: meta,
	})
	if err != nil {
		t.Fatalf("create %s: %v", r.name, err)
	}
	if r.closed {
		if err := store.Close(b.ID); err != nil {
			t.Fatalf("close %s: %v", r.name, err)
		}
	}
}

func TestLatestEphemeralTerminalFailure(t *testing.T) {
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return base.Add(d).Format(time.RFC3339) }
	const dir = "/work/a"
	row := func(name string, mut func(*terminalHistoryRow)) terminalHistoryRow {
		r := terminalHistoryRow{
			name: name, template: "worker", origin: "ephemeral",
			workerDir: dir, reason: "work_dir_collision", at: at(-time.Minute), closed: true,
		}
		if mut != nil {
			mut(&r)
		}
		return r
	}

	tests := []struct {
		name     string
		rows     []terminalHistoryRow
		notAfter time.Time
		want     time.Time
		wantOK   bool
	}{
		{name: "no_history"},
		{name: "closed_row_found", rows: []terminalHistoryRow{row("c", nil)}, want: base.Add(-time.Minute), wantOK: true},
		{name: "open_row_found", rows: []terminalHistoryRow{row("o", func(r *terminalHistoryRow) { r.closed = false })}, want: base.Add(-time.Minute), wantOK: true},
		{
			name: "latest_timestamp_wins_regardless_of_store_order",
			rows: []terminalHistoryRow{
				row("recent", func(r *terminalHistoryRow) { r.at = at(-30 * time.Second) }),
				row("old", func(r *terminalHistoryRow) { r.at = at(-10 * time.Minute) }),
			},
			want: base.Add(-30 * time.Second), wantOK: true,
		},
		{name: "other_template", rows: []terminalHistoryRow{row("x", func(r *terminalHistoryRow) { r.template = "other" })}},
		{name: "manual_origin", rows: []terminalHistoryRow{row("x", func(r *terminalHistoryRow) { r.origin = "manual" })}},
		{name: "named_origin", rows: []terminalHistoryRow{row("x", func(r *terminalHistoryRow) { r.origin = "named" })}},
		{name: "other_work_dir", rows: []terminalHistoryRow{row("x", func(r *terminalHistoryRow) { r.workerDir = "/work/b" })}},
		{
			name: "canonical_worker_dir_precedes_legacy",
			rows: []terminalHistoryRow{row("x", func(r *terminalHistoryRow) { r.workerDir = "/work/b"; r.workDir = dir })},
		},
		{
			name: "legacy_work_dir_used_when_worker_dir_absent",
			rows: []terminalHistoryRow{row("x", func(r *terminalHistoryRow) { r.workerDir = ""; r.workDir = dir })},
			want: base.Add(-time.Minute), wantOK: true,
		},
		{name: "no_terminal_reason", rows: []terminalHistoryRow{row("x", func(r *terminalHistoryRow) { r.reason = "" })}},
		{name: "missing_timestamp_skipped", rows: []terminalHistoryRow{row("x", func(r *terminalHistoryRow) { r.at = "" })}},
		{name: "unparseable_timestamp_skipped", rows: []terminalHistoryRow{row("x", func(r *terminalHistoryRow) { r.at = "yesterday" })}},
		{
			name: "future_timestamp_ignored_in_favor_of_earlier_valid_row",
			rows: []terminalHistoryRow{
				row("future", func(r *terminalHistoryRow) { r.at = at(2 * time.Hour) }),
				row("valid", func(r *terminalHistoryRow) { r.at = at(-2 * time.Minute) }),
			},
			notAfter: base.Add(time.Minute),
			want:     base.Add(-2 * time.Minute), wantOK: true,
		},
		{
			name:     "only_future_timestamp_reports_none",
			rows:     []terminalHistoryRow{row("future", func(r *terminalHistoryRow) { r.at = at(2 * time.Hour) })},
			notAfter: base.Add(time.Minute),
		},
		{
			name:     "timestamp_equal_to_bound_is_kept",
			rows:     []terminalHistoryRow{row("edge", func(r *terminalHistoryRow) { r.at = at(time.Minute) })},
			notAfter: base.Add(time.Minute),
			want:     base.Add(time.Minute), wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := beads.NewMemStore()
			for _, r := range tt.rows {
				seedTerminalHistory(t, store, r)
			}
			notAfter := tt.notAfter
			if notAfter.IsZero() {
				notAfter = base.Add(time.Hour)
			}
			got, ok, err := NewStore(beads.SessionStore{Store: store}).LatestEphemeralTerminalFailure("worker", dir, notAfter)
			if err != nil {
				t.Fatalf("LatestEphemeralTerminalFailure: %v", err)
			}
			if ok != tt.wantOK || !got.Equal(tt.want) {
				t.Fatalf("got (%v, %v), want (%v, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestLatestEphemeralTerminalFailureNilStore(t *testing.T) {
	var s *Store
	if _, ok, err := s.LatestEphemeralTerminalFailure("worker", "/work/a", time.Now()); ok || err != nil {
		t.Fatalf("nil store = (ok %v, err %v), want (false, nil)", ok, err)
	}
}

type terminalHistoryFailStore struct {
	beads.Store
	err error
}

func (s terminalHistoryFailStore) List(beads.ListQuery) ([]beads.Bead, error) { return nil, s.err }
func (s terminalHistoryFailStore) ListByLabel(string, int, ...beads.QueryOpt) ([]beads.Bead, error) {
	return nil, s.err
}

func (s terminalHistoryFailStore) ListByMetadata(map[string]string, int, ...beads.QueryOpt) ([]beads.Bead, error) {
	return nil, s.err
}

func TestLatestEphemeralTerminalFailureSurfacesStoreError(t *testing.T) {
	sentinel := errors.New("history unavailable")
	store := terminalHistoryFailStore{Store: beads.NewMemStore(), err: sentinel}
	_, ok, err := NewStore(beads.SessionStore{Store: store}).LatestEphemeralTerminalFailure("worker", "/work/a", time.Now())
	if ok {
		t.Fatal("ok = true on a failed read")
	}
	if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "worker") {
		t.Fatalf("err = %v, want wrapped sentinel naming the template", err)
	}
}
