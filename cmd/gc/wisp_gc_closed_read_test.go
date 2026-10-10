package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// The orphan reaper's read of closed wisp-tier rows goes to bd as two bd
// query reads (no-history and ephemeral) and never as the whole-closed-set
// bd list --status=closed --all, which lists every closed issue in the store
// on every reaper pass.
func TestReapOrphanedClosedWispsNeverListsTheWholeClosedSet(t *testing.T) {
	var calls []string
	runner := func(_, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return []byte(`[]`), nil
	}
	store := beads.NewBdStore("/city", runner)

	if _, err := reapOrphanedClosedWisps(store, time.Now().Add(-24*time.Hour), 10); err != nil {
		t.Fatalf("reapOrphanedClosedWisps: %v", err)
	}

	var sawNoHistory, sawEphemeral bool
	for _, c := range calls {
		if strings.HasPrefix(c, "bd list ") && strings.Contains(c, "--status=closed") && strings.Contains(c, "--all") {
			t.Fatalf("wisp gc issued the whole-closed-set read %q", c)
		}
		sawNoHistory = sawNoHistory || (strings.HasPrefix(c, "bd query ") && strings.Contains(c, "no_history=true AND status=closed"))
		sawEphemeral = sawEphemeral || (strings.HasPrefix(c, "bd query ") && strings.Contains(c, "ephemeral=true AND status=closed"))
	}
	if !sawNoHistory || !sawEphemeral {
		t.Fatalf("calls = %#v, want the closed no-history and ephemeral bd query reads", calls)
	}
}
