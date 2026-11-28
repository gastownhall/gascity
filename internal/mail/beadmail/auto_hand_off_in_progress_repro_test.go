package beadmail

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
)

// An auto-handoff wisp that left "open" (in_progress) must stay visible to the
// inject path, so `gc mail check --inject` delivers and archives it instead of
// leaving it to starve the session forever (ga-gfigo2, #6211).
func TestCheckAutoHandoffsSeesInProgressHandoffWisp(t *testing.T) {
	store := beads.NewMemStore()
	p := newTestProvider(store)
	auto, _, err := p.SendHandoff(mail.HandoffIntent{
		From:        "worker",
		To:          "worker",
		Subject:     "context cycle",
		Body:        "continue durable work",
		ThreadID:    "thread-auto",
		ExtraLabels: []string{mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel},
	})
	if err != nil {
		t.Fatalf("SendHandoff auto: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(auto.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("set in_progress: %v", err)
	}

	got, err := p.CheckAutoHandoffs([]string{"worker"})
	if err != nil {
		t.Fatalf("CheckAutoHandoffs: %v", err)
	}
	found := false
	for _, m := range got {
		if m.ID == auto.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("CheckAutoHandoffs(worker) = %d messages, missing in_progress auto-handoff wisp %s: inject can never deliver or archive it", len(got), auto.ID)
	}
}
