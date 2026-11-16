package opjournal

import (
	"context"
	"testing"
	"time"
)

// viewRecordingExec is a fakeExec that records every view it is probed with.
type viewRecordingExec struct {
	*fakeExec
	views []ConversationView
}

func (e *viewRecordingExec) Probe(ctx context.Context, conv ConversationView) (Probe, error) {
	e.views = append(e.views, conv)
	return e.fakeExec.Probe(ctx, conv)
}

// TestLanePassHandsProbeTheContentAge: the view a pass probes with carries
// when its oldest content candidate became deliverable (its creation, or its
// not_before when later), which a keystroke executor's starvation rule reads;
// content that is not yet due is no candidate.
func TestLanePassHandsProbeTheContentAge(t *testing.T) {
	setup := func(t *testing.T) (*laneHarness, func() time.Time) {
		t.Helper()
		h := newLaneHarness(t)
		l := h.lane(t, "host-a")
		exec := &viewRecordingExec{fakeExec: newFakeExec(h.clk)}
		exec.probe.Boundary = TriNo
		return h, func() time.Time {
			t.Helper()
			before := len(exec.views)
			if _, err := l.Pass(context.Background(), view(), exec); err != nil {
				t.Fatalf("Pass: %v", err)
			}
			if len(exec.views) != before+1 {
				t.Fatalf("pass probed %d times; want once", len(exec.views)-before)
			}
			return exec.views[len(exec.views)-1].ContentSince
		}
	}
	t.Run("the oldest due candidate", func(t *testing.T) {
		h, pass := setup(t)
		older := h.propose(t, laneMsg("k-older", RolePrompt, "older", withWhen(WhenBoundary)))
		h.clk.Advance(30 * time.Second)
		h.propose(t, laneMsg("k-younger", RolePrompt, "younger", withWhen(WhenBoundary)))
		h.propose(t, laneMsg("k-later", RolePrompt, "not yet due", withWhen(WhenBoundary), func(r *Request) { r.NotBefore = h.clk.Now().Add(time.Hour) }))
		if got := pass(); !got.Equal(older.CreatedAt) {
			t.Fatalf("ContentSince %v; want the oldest due op's creation %v", got, older.CreatedAt)
		}
	})
	t.Run("not_before later than creation", func(t *testing.T) {
		h, pass := setup(t)
		deferred := h.clk.Now().Add(time.Minute)
		h.propose(t, laneMsg("k-deferred", RolePrompt, "deferred", withWhen(WhenBoundary), func(r *Request) { r.NotBefore = deferred }))
		h.clk.Advance(2 * time.Minute)
		if got := pass(); !got.Equal(deferred) {
			t.Fatalf("ContentSince %v; want the op's not_before %v", got, deferred)
		}
	})
	if !view().ContentSince.IsZero() {
		t.Fatal("the host's view carries a content age")
	}
}
