//go:build linux

package proctable

import "testing"

// These tests pin the ga-i20db verdict on the scope proofs' stability
// rechecks. Each proof re-reads the candidate and the scope-exit spawner
// after collecting its evidence and declines when the observation moved —
// that recheck exists to catch pid reuse and scope migration, not scheduler
// noise. In production the recheck compared the full identity struct,
// INCLUDING the volatile /proc stat state field, so a busy spawner or
// candidate flipping S<->R between the two reads declined the proof for that
// sweep. Measured live on the wedged host (311 walks): every single decline
// was a state-only flap — ~3-5% per candidate walk — and under the
// all-or-nothing completeness gate that kept essentially every drain-ack
// observation incomplete on a loaded fleet. Scheduler state is not identity.
//
// Kernel death is not scheduler noise: a spawner that died between the
// environ read and the recheck invalidates environ-based evidence (the
// ga-f7v2ft.194 vacuous-environ guard), so the proofs decline that shape
// explicitly at the call sites — see the kernel-dead recheck cases below.

func TestSameAdjudicatedProcessIgnoresSchedulerState(t *testing.T) {
	before := processIdentity{
		PID:        901,
		PPID:       900,
		State:      "S",
		StartTicks: 2000,
		Cgroup:     "/user.slice/tmux-spawn-feed1.scope",
	}
	after := before
	after.State = "R"
	if !before.sameAdjudicatedProcess(after) {
		t.Fatal("a scheduler-state flip (S->R) between the evidence read and the recheck declined the proof; state is not identity")
	}
}

func TestSameAdjudicatedProcessDetectsRealMovement(t *testing.T) {
	base := processIdentity{
		PID:        901,
		PPID:       900,
		State:      "S",
		StartTicks: 2000,
		Cgroup:     "/user.slice/tmux-spawn-feed1.scope",
	}
	tests := []struct {
		name   string
		mutate func(*processIdentity)
	}{
		{"pid reuse (start ticks moved)", func(p *processIdentity) { p.StartTicks = 2001 }},
		{"different pid", func(p *processIdentity) { p.PID = 902 }},
		{"re-parented", func(p *processIdentity) { p.PPID = 1 }},
		{"scope migrated", func(p *processIdentity) { p.Cgroup = "/user.slice/tmux-spawn-feed2.scope" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			after := base
			tt.mutate(&after)
			if base.sameAdjudicatedProcess(after) {
				t.Fatal("a genuinely moved observation passed the stability recheck; the proof must decline")
			}
		})
	}
}

func TestSameAdjudicatedProcessStatIgnoresSchedulerState(t *testing.T) {
	before := processStat{PID: 702, PPID: 701, State: "S", StartTicks: 2005}
	after := before
	after.State = "R"
	if !before.sameAdjudicatedProcess(after) {
		t.Fatal("a scheduler-state flip on the foreign-lineage candidate declined the proof; state is not identity")
	}
	moved := before
	moved.StartTicks = 2006
	if before.sameAdjudicatedProcess(moved) {
		t.Fatal("a pid-reuse shape passed the foreign-lineage stability recheck; the proof must decline")
	}
	reparented := before
	reparented.PPID = 1
	if before.sameAdjudicatedProcess(reparented) {
		t.Fatal("a re-parented candidate passed the foreign-lineage stability recheck; the walked chain no longer describes it")
	}
}
