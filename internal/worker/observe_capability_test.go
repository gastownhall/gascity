package worker

import (
	"context"
	"testing"
)

type liveObservationOnlyHandle struct {
	observation LiveObservation
	err         error
}

func (h liveObservationOnlyHandle) LiveObservation(context.Context) (LiveObservation, error) {
	return h.observation, h.err
}

func TestObserveHandleAcceptsLiveObservationCapability(t *testing.T) {
	want := LiveObservation{
		Running:     true,
		SessionID:   "sess-1",
		SessionName: "worker-1",
	}

	got, err := ObserveHandle(context.Background(), liveObservationOnlyHandle{observation: want})
	if err != nil {
		t.Fatalf("ObserveHandle: %v", err)
	}
	if got != want {
		t.Fatalf("ObserveHandle = %+v, want %+v", got, want)
	}
}
