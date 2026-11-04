package tmux

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// A pane this process armed after the state cache's process table was
// scanned has a tap the table cannot hold yet. The inventory, listed fresh,
// shows the new nonce, but its tap liveness comes from that older table:
// judged by the table alone the pane read reader_lost until the next refresh
// (~2 s), which is when readiness is decided (the 2026-11-03 shadow: 35% of
// readiness points reader_lost, ga-f4ovjm.65). The tap's liveness is unknown
// instead: the reading is valid but unverified. A table scanned after the
// arm that still lacks the tap reads reader_lost as before.
func TestTapArmedAfterSnapshotIsNotReaderLost(t *testing.T) {
	tm, rec, _, _ := captureTestTmux(t)
	table := tapTable() // no tap at all: the arm has not run yet
	p := &Provider{tm: tm, cfg: tm.cfg, workDirs: map[string]string{}}
	p.cache = NewStateCache(&tmuxFetcher{tm: tm, snapshot: func(context.Context) (processSnapshot, error) { return table, nil }}, time.Hour)

	// The cache scans before the arm.
	rec.out = fetchRows(capturePane{mark: "agent", id: "%5", pipe: "0"})
	if r, err := p.ProgramStatus("s"); err != nil || r.Validity != runtime.ProgramStatusNotArmed {
		t.Fatalf("before the arm: ProgramStatus = (%+v, %v), want not_armed", r, err)
	}

	// The arm.
	rec.out = "%5"
	if err := tm.respawnPaneWithWorkDir("%5", "/w", "codex", runtime.ProgramStatusArm{Capture: true}); err != nil {
		t.Fatalf("respawn: %v", err)
	}
	armed := rec.calls[len(rec.calls)-1]
	i := slices.Index(armed, programTapOption)
	if i < 0 {
		t.Fatalf("respawn did not arm: %q", armed)
	}
	nonce := strings.TrimSuffix(armed[i+1], ".#{pane_id}") + ".%5"
	pane := capturePane{mark: "agent", id: "%5", pipe: "1", tap: nonce, record: psRecord(t, nonce, runtime.ProgramWorking)}

	// The fresh inventory names the new nonce; the cached table predates it.
	rec.out = inventoryLines(inventoryRows("s", pane)...)
	entries, err := p.RuntimeInventory(context.Background())
	if err != nil {
		t.Fatalf("RuntimeInventory: %v", err)
	}
	if s := entries["s"].Program; s.Validity == runtime.ProgramStatusReaderLost || !s.Running() || s.ReaderVerified {
		t.Fatalf("pane armed after the cached scan reads %+v, want its working record, unverified, not reader_lost", s)
	}
	if s := tm.snapshotTaps(p.cache.currentState()); s.liveness("0ther0ne.%5") != runtime.ProgramTapGone {
		t.Fatalf("a nonce this process never armed reads %v against the table, want gone", s.liveness("0ther0ne.%5"))
	}

	// A table scanned after the arm that lacks the tap: the reader is lost.
	p.cache.Invalidate()
	rec.out = fetchRows(pane)
	if r, err := p.ProgramStatus("s"); err != nil || r.Validity != runtime.ProgramStatusReaderLost {
		t.Fatalf("table scanned after the arm, no tap: ProgramStatus = (%+v, %v), want reader_lost", r, err)
	}
	// And one holding it verifies the reader.
	table = tapTable(nonce)
	p.cache.Invalidate()
	if r, err := p.ProgramStatus("s"); err != nil || !r.Running() || !r.ReaderVerified {
		t.Fatalf("table holding the tap: ProgramStatus = (%+v, %v), want a verified working reading", r, err)
	}
}

// Kills a stamp map that grows without bound, or forgets an arm a trusted
// snapshot may still predate: stamps live programArmRetention, and
// armedSince returns only the arms at or after the scan.
func TestProgramArmStampsRetention(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := &programArmStamps{now: func() time.Time { return now }}
	s.noteArmed(&capturePlan{rand: "old"})
	now = now.Add(time.Second)
	scanned := now
	s.noteArmed(&capturePlan{rand: "fresh"})
	s.noteArmed(nil)

	if got := s.armedSince(scanned); len(got) != 1 {
		t.Fatalf("armedSince(scan) = %v, want only the arm at or after the scan", got)
	} else if _, ok := got["fresh"]; !ok {
		t.Fatalf("armedSince(scan) = %v, want fresh", got)
	}
	if got := s.armedSince(time.Time{}); len(got) != 2 {
		t.Fatalf("armedSince(unknown scan) = %v, want every stamp held", got)
	}

	now = now.Add(programArmRetention + time.Second)
	s.noteArmed(&capturePlan{rand: "later"})
	if len(s.at) != 1 {
		t.Fatalf("stamps after the retention = %v, want only the newest", s.at)
	}
}

// nonceRand reads only a "<rand>.<pane_id>" nonce.
func TestNonceRand(t *testing.T) {
	for nonce, want := range map[string]string{
		"rAnd0m.%5": "rAnd0m",
		"rAnd0m":    "",
		".%5":       "",
		"rAnd0m.5":  "",
		"":          "",
	} {
		if got := nonceRand(nonce); got != want {
			t.Errorf("nonceRand(%q) = %q, want %q", nonce, got, want)
		}
	}
}
