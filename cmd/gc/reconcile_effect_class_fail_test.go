package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/session"
)

// TestClassOnlyAbandonLands: a Fail step whose only write is a cell's keys
// (Class) lands them and settles failed, as a Fail with a Write does.
func TestClassOnlyAbandonLands(t *testing.T) {
	k := newTxKit(t)
	abandon := effectSpec{needs: needs{NameLock: true}, sections: []section{{Decide: func(v txView) txStep {
		return txStep{Class: session.CellInfo(v.Row, session.ActorController, session.DispEvent{Kind: session.EvQuarantine, Until: v.Now.Add(time.Hour)}, v.Now), Fail: "gave-up"}
	}}}}
	s := k.run(context.Background(), abandon)
	t.Logf("settlement %+v quarantined_until=%q", s, k.meta("quarantined_until"))
	if s.Outcome != settledFailed || k.meta("quarantined_until") == "" {
		t.Errorf("a class-only abandon settled %v (cause %q) and wrote quarantined_until=%q; want failed with the cell's write landed", s.Outcome, s.Cause, k.meta("quarantined_until"))
	}
}

// TestClassWithWriteAbandonLands: the same abandon with a lifecycle key
// beside the class lands both.
func TestClassWithWriteAbandonLands(t *testing.T) {
	k := newTxKit(t)
	abandon := effectSpec{needs: needs{NameLock: true}, sections: []section{{Decide: func(v txView) txStep {
		return txStep{Write: session.MetadataPatch{"wake_attempts": "1"}, Class: session.CellInfo(v.Row, session.ActorController, session.DispEvent{Kind: session.EvQuarantine, Until: v.Now.Add(time.Hour)}, v.Now), Fail: "gave-up"}
	}}}}
	s := k.run(context.Background(), abandon)
	if s.Outcome != settledFailed || k.meta("quarantined_until") == "" || k.meta("wake_attempts") != "1" {
		t.Errorf("an abandon with a write settled %v, quarantined_until=%q wake_attempts=%q", s.Outcome, k.meta("quarantined_until"), k.meta("wake_attempts"))
	}
}
