package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestObserveLivenessTracksZombieProcess(t *testing.T) {
	sp := NewFake()
	if err := sp.Start(context.Background(), "worker", Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sp.Zombies["worker"] = true

	got := ObserveLiveness(sp, "worker", []string{"agent-cli"})
	if !got.Running {
		t.Fatalf("ObserveLiveness.Running = false, want true for present runtime")
	}
	if got.Alive {
		t.Fatalf("ObserveLiveness.Alive = true, want false for zombie process")
	}
}

func TestObserveLivenessWithoutProcessNamesTreatsRunningAsAlive(t *testing.T) {
	sp := NewFake()
	if err := sp.Start(context.Background(), "worker", Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sp.Zombies["worker"] = true

	got := ObserveLiveness(sp, "worker", nil)
	if !got.Running || !got.Alive {
		t.Fatalf("ObserveLiveness() = %#v, want running+alive when no process names are configured", got)
	}
}

func TestObserveLivenessPromotesRunningWhenProcessCheckFindsFalseNegative(t *testing.T) {
	base := NewFake()
	if err := base.Start(context.Background(), "worker", Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sp := falseNegativeLivenessProvider{Fake: base}

	got := ObserveLiveness(sp, "worker", []string{"agent-cli"})
	if !got.Running || !got.Alive {
		t.Fatalf("ObserveLiveness() = %#v, want process liveness to recover IsRunning false negative", got)
	}
}

type falseNegativeLivenessProvider struct {
	*Fake
}

func (p falseNegativeLivenessProvider) IsRunning(name string) bool {
	_ = p.Fake.IsRunning(name)
	return false
}

func TestObserveLivenessBoundedFallsBackToObserveLivenessWithoutRicherInterface(t *testing.T) {
	sp := NewFake()
	if err := sp.Start(context.Background(), "worker", Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sp.Zombies["worker"] = true

	want := ObserveLiveness(sp, "worker", []string{"agent-cli"})

	got, status, err := ObserveLivenessBounded(context.Background(), sp, "worker", []string{"agent-cli"}, time.Second)
	if err != nil {
		t.Fatalf("ObserveLivenessBounded() error = %v, want nil", err)
	}
	if status != ObservationComplete {
		t.Fatalf("ObservationStatus = %v, want ObservationComplete", status)
	}
	if got != want {
		t.Fatalf("ObserveLivenessBounded() = %#v, want %#v (parity with ObserveLiveness fallback)", got, want)
	}
}

func TestObserveLivenessBoundedForwardsExistingLivenessObserver(t *testing.T) {
	base := NewFake()
	if err := base.Start(context.Background(), "worker", Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sp := falseNegativeLivenessProvider{Fake: base}

	got, status, err := ObserveLivenessBounded(context.Background(), sp, "worker", []string{"agent-cli"}, time.Second)
	if err != nil {
		t.Fatalf("ObserveLivenessBounded() error = %v, want nil", err)
	}
	if status != ObservationComplete {
		t.Fatalf("ObservationStatus = %v, want ObservationComplete", status)
	}
	if !got.Running || !got.Alive {
		t.Fatalf("ObserveLivenessBounded() = %#v, want process liveness to recover IsRunning false negative", got)
	}
}

func TestObserveLivenessBoundedMapsRuntimeUnavailableToIncomplete(t *testing.T) {
	base := NewFake()
	if err := base.Start(context.Background(), "worker", Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	wantErr := fmt.Errorf("tmux server unreachable: %w", ErrRuntimeUnavailable)
	sp := errorLivenessProvider{Fake: base, liveness: Liveness{Running: true}, err: wantErr}

	got, status, err := ObserveLivenessBounded(context.Background(), sp, "worker", []string{"agent-cli"}, time.Second)
	if status != ObservationIncomplete {
		t.Fatalf("ObservationStatus = %v, want ObservationIncomplete for a result wrapping ErrRuntimeUnavailable", status)
	}
	if !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("ObserveLivenessBounded() error = %v, want errors.Is(err, ErrRuntimeUnavailable)", err)
	}
	if !got.Running {
		t.Fatalf("ObserveLivenessBounded() Liveness = %#v, want the provider's own observation preserved alongside Incomplete", got)
	}
}

func TestObserveLivenessBoundedCompleteWithNonRuntimeErrorStaysComplete(t *testing.T) {
	base := NewFake()
	if err := base.Start(context.Background(), "worker", Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	wantErr := errors.New("boom")
	sp := errorLivenessProvider{Fake: base, liveness: Liveness{Running: true, Alive: true}, err: wantErr}

	got, status, err := ObserveLivenessBounded(context.Background(), sp, "worker", []string{"agent-cli"}, time.Second)
	if status != ObservationComplete {
		t.Fatalf("ObservationStatus = %v, want ObservationComplete for an error not wrapping ErrRuntimeUnavailable", status)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("ObserveLivenessBounded() error = %v, want %v", err, wantErr)
	}
	if !got.Running || !got.Alive {
		t.Fatalf("ObserveLivenessBounded() Liveness = %#v, want the provider's own observation preserved", got)
	}
}

func TestObserveLivenessBoundedTimesOutToIncompleteWithZeroLiveness(t *testing.T) {
	base := NewFake()
	if err := base.Start(context.Background(), "worker", Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	sp := blockingLivenessProvider{Fake: base, release: release}

	got, status, err := ObserveLivenessBounded(context.Background(), sp, "worker", []string{"agent-cli"}, 20*time.Millisecond)
	if status != ObservationIncomplete {
		t.Fatalf("ObservationStatus = %v, want ObservationIncomplete on timeout", status)
	}
	if got != (Liveness{}) {
		t.Fatalf("ObserveLivenessBounded() Liveness = %#v, want zero value on timeout", got)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ObserveLivenessBounded() error = %v, want errors.Is(err, context.DeadlineExceeded)", err)
	}
}

type errorLivenessProvider struct {
	*Fake
	liveness Liveness
	err      error
}

func (p errorLivenessProvider) ObserveLivenessWithError(_ string, _ []string) (Liveness, error) {
	return p.liveness, p.err
}

type blockingLivenessProvider struct {
	*Fake
	release <-chan struct{}
}

func (p blockingLivenessProvider) ObserveLivenessWithError(_ string, _ []string) (Liveness, error) {
	<-p.release
	return Liveness{Running: true, Alive: true}, nil
}
