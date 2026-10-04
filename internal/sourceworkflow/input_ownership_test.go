package sourceworkflow

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convoy"
)

func inputOwnershipTestSetup(t *testing.T) (InputOwnership, beads.Bead, beads.Bead) {
	t.Helper()
	work := beads.NewMemStore()
	member, err := work.Create(beads.Bead{Title: "work"})
	if err != nil {
		t.Fatal(err)
	}
	input, err := work.Create(beads.Bead{Title: "input", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := convoy.TrackItem(work, input.ID, member.ID); err != nil {
		t.Fatal(err)
	}
	return InputOwnership{Work: work, Graph: beads.NewMemStore(), CityPath: t.TempDir(), LockScope: "city:test", Target: "executor"}, member, input
}

func TestInputOwnershipPartialAcquireRollsBack(t *testing.T) {
	o, first, input := inputOwnershipTestSetup(t)
	second, err := o.Work.Create(beads.Bead{Title: "claimed", Assignee: "direct-session"})
	if err != nil {
		t.Fatal(err)
	}
	if err := convoy.TrackItem(o.Work, input.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	if err := o.WithWorkflow(context.Background(), input.ID, func() error { t.Fatal("conflicting launch executed"); return nil }); err == nil {
		t.Fatal("want conflict")
	}
	b, err := o.Work.Get(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Assignee != "" {
		t.Fatalf("partial acquire retained %q", b.Assignee)
	}
	b, err = o.Work.Get(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Assignee != "direct-session" {
		t.Fatalf("rollback stole competing owner %q", b.Assignee)
	}
}

func TestInputOwnershipRollbackDoesNotClearSuccessor(t *testing.T) {
	o, member, input := inputOwnershipTestSetup(t)
	wantErr := errors.New("failed launch")
	err := o.WithWorkflow(context.Background(), input.ID, func() error {
		owner := "successor"
		if err := o.Work.Update(member.ID, beads.UpdateOpts{Assignee: &owner}); err != nil {
			t.Fatal(err)
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v", err)
	}
	b, err := o.Work.Get(member.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Assignee != "successor" {
		t.Fatalf("rollback cleared successor: %+v", b)
	}
}

func TestInputOwnershipOnlyLastTerminalGraphReleasesInput(t *testing.T) {
	o, member, input := inputOwnershipTestSetup(t)
	var roots []beads.Bead
	for range 2 {
		if err := o.WithWorkflow(context.Background(), input.ID, func() error {
			root, err := o.Graph.Create(beads.Bead{Title: "graph", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.InputConvoyIDMetadataKey: input.ID}})
			roots = append(roots, root)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.Graph.Close(roots[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := o.ReleaseTerminal(context.Background(), input.ID); err != nil {
		t.Fatal(err)
	}
	if err := o.WithDirect(context.Background(), member.ID, func() error { t.Fatal("direct executor ran with a live shared graph"); return nil }); err == nil {
		t.Fatal("want conflict")
	}
	if err := o.Graph.Close(roots[1].ID); err != nil {
		t.Fatal(err)
	}
	if err := o.WithDirect(context.Background(), member.ID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	b, err := o.Work.Get(member.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Assignee != "" || b.Status != member.Status {
		t.Fatalf("terminal recovery retained workflow owner: %+v", b)
	}
}

func TestInputOwnershipUnsupportedWritesFailClosed(t *testing.T) {
	o, member, input := inputOwnershipTestSetup(t)
	o.Work.(*beads.MemStore).DisableConditionalWrites = true
	err := o.WithWorkflow(context.Background(), input.ID, func() error { t.Fatal("unfenced launch executed"); return nil })
	if !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
		t.Fatalf("error = %v", err)
	}
	b, err := o.Work.Get(member.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Assignee != "" {
		t.Fatalf("unfenced acquisition changed input: %+v", b)
	}
}
