package tmux

import (
	"context"
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
// arm that still lacks the tap reads reader_lost as before. Every arm site
// stamps its arm: a create's list, a respawn, heal's arm of an idle pane and
// heal's replacement of its own tap. Kills a stamp missing at any of them.
func TestTapArmedAfterSnapshotIsNotReaderLost(t *testing.T) {
	want := runtime.ProgramStatusArm{Capture: true}
	healed := healPane{programPane: programPane{mark: "agent", id: "%5"}, target: "=s:@1.%5"}
	for _, site := range []struct {
		name string
		arm  func(*Tmux, *stagedRecorder) error
	}{
		{"create", func(tm *Tmux, rec *stagedRecorder) error {
			rec.out, rec.outs = "", []string{"%5"}
			return tm.newSessionWithCommandAndEnv("s", "/w", "codex", nil, want)
		}},
		{"respawn", func(tm *Tmux, rec *stagedRecorder) error {
			rec.out = "%5"
			return tm.respawnPaneWithWorkDir("%5", "/w", "codex", want)
		}},
		{"heal arms an idle pane", func(tm *Tmux, _ *stagedRecorder) error {
			_, err := tm.armIdlePane(healed, want)
			return err
		}},
		{"heal replaces its tap", func(tm *Tmux, _ *stagedRecorder) error {
			_, err := tm.replaceTap(healed, want)
			return err
		}},
	} {
		t.Run(site.name, func(t *testing.T) {
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
			before := len(rec.calls)
			if err := site.arm(tm, rec); err != nil {
				t.Fatalf("arm: %v", err)
			}
			nonce := armedRand(t, rec.calls[before:]) + ".%5"
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
		})
	}
}

// armedRand is the nonce rand the last capture arm among calls set: the
// "<rand>.#{pane_id}" value of its tap-option set, an argv word or inside an
// if-shell arm list.
func armedRand(t *testing.T, calls [][]string) string {
	t.Helper()
	for i := len(calls) - 1; i >= 0; i-- {
		for j, arg := range calls[i] {
			if m := tapRandRe.FindStringSubmatch(arg); m != nil {
				return m[1]
			}
			if arg != programTapOption || j+1 == len(calls[i]) {
				continue
			}
			if rand, ok := strings.CutSuffix(calls[i][j+1], ".#{pane_id}"); ok {
				return rand
			}
		}
	}
	t.Fatalf("no capture arm among %q", calls)
	return ""
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
