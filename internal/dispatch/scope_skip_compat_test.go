package dispatch

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// Beads 1.3 guards update --status closed, while CloseAll is the explicit
// forced-close operation used when work is skipped rather than completed.
type guardedScopeSkipStore struct{ *beads.MemStore }

func (s *guardedScopeSkipStore) Update(id string, opts beads.UpdateOpts) error {
	if opts.Status != nil && *opts.Status == "closed" {
		deps, err := s.DepList(id, "down")
		if err != nil {
			return err
		}
		for _, dep := range deps {
			blocker, err := s.Get(dep.DependsOnID)
			if err != nil {
				return err
			}
			if dep.Type == "blocks" && blocker.Status != "closed" {
				return fmt.Errorf("cannot close blocked issue: %s", id)
			}
		}
	}
	return s.MemStore.Update(id, opts)
}

func TestSkipScopeMembersClosesBlockedMember(t *testing.T) {
	store := &guardedScopeSkipStore{beads.NewMemStore()}
	control := mustCreateWorkflowBead(t, store, beads.Bead{Title: "abort control"})
	member := mustCreateWorkflowBead(t, store, beads.Bead{Title: "skipped member"})
	mustDepAdd(t, store, member.ID, control.ID, "blocks")
	closed, err := skipScopeMembers(store, []string{member.ID})
	if err != nil || closed != 1 {
		t.Fatalf("skip blocked member = %d, %v", closed, err)
	}
	got := mustGetBead(t, store, member.ID)
	if got.Status != "closed" || got.Metadata["gc.outcome"] != "skipped" {
		t.Fatalf("skipped member = %+v", got)
	}
	if len(got.Metadata["close_reason"]) < 20 {
		t.Fatalf("skip must carry a descriptive close reason: %+v", got.Metadata)
	}
	if got := mustGetBead(t, store, control.ID); got.Status != "open" {
		t.Fatalf("in-flight abort control must remain open: %+v", got)
	}
	deps, err := store.DepList(member.ID, "down")
	if err != nil || len(deps) != 1 || deps[0].DependsOnID != control.ID {
		t.Fatalf("skip must preserve dependency: %+v, %v", deps, err)
	}
}
