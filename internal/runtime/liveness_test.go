package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

var errLivenessUnavailableForTest = errors.New("liveness unavailable")

type errorBearingLivenessProvider struct {
	*Fake
	observation Liveness
	err         error
}

func (p *errorBearingLivenessProvider) ObserveLivenessWithError(string, []string) (Liveness, error) {
	return p.observation, p.err
}

func TestObserveLivenessWithErrorPreservesOptionalProviderFailure(t *testing.T) {
	sp := &errorBearingLivenessProvider{
		Fake: NewFake(),
		err:  errLivenessUnavailableForTest,
	}

	got, err := ObserveLivenessWithError(sp, "worker", []string{"agent-cli"})
	if !errors.Is(err, errLivenessUnavailableForTest) {
		t.Fatalf("ObserveLivenessWithError error = %v, want %v", err, errLivenessUnavailableForTest)
	}
	if got != (Liveness{}) {
		t.Fatalf("ObserveLivenessWithError observation = %+v, want zero on unavailable observation", got)
	}
}

func TestObserveLivenessWithErrorKeepsLegacyProviderCompatible(t *testing.T) {
	sp := NewFake()
	if err := sp.Start(context.Background(), "worker", Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	got, err := ObserveLivenessWithError(sp, "worker", nil)
	if err != nil {
		t.Fatalf("ObserveLivenessWithError legacy provider: %v", err)
	}
	if !got.Running || !got.Alive {
		t.Fatalf("ObserveLivenessWithError legacy provider = %+v, want running+alive", got)
	}
}

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

func TestObserveLivenessBoundedNormalizesObserverLiveness(t *testing.T) {
	base := NewFake()
	if err := base.Start(context.Background(), "worker", Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sp := errorLivenessProvider{Fake: base, liveness: Liveness{Running: false, Alive: true}}

	got, status, err := ObserveLivenessBounded(context.Background(), sp, "worker", []string{"agent-cli"}, time.Second)
	if err != nil {
		t.Fatalf("ObserveLivenessBounded() error = %v, want nil", err)
	}
	if status != ObservationComplete {
		t.Fatalf("ObservationStatus = %v, want ObservationComplete", status)
	}
	if want := (Liveness{Running: true, Alive: true}); got != want {
		t.Fatalf("ObserveLivenessBounded() = %#v, want %#v (Alive must promote Running, same as every other entry point)", got, want)
	}
}

func TestObserveLivenessBoundedCancelledParentContextIsIncomplete(t *testing.T) {
	base := NewFake()
	if err := base.Start(context.Background(), "worker", Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, status, err := ObserveLivenessBounded(ctx, base, "worker", []string{"agent-cli"}, time.Second)
	if status != ObservationIncomplete {
		t.Fatalf("ObservationStatus = %v, want ObservationIncomplete for an already-canceled parent context", status)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ObserveLivenessBounded() error = %v, want errors.Is(err, context.Canceled)", err)
	}
	if got != (Liveness{}) {
		t.Fatalf("ObserveLivenessBounded() Liveness = %#v, want zero value when no observation ran", got)
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

// awaitObservationRepeats is how often the waiting-answer tests re-run the
// final select. The old two-case select took the ctx.Done case about half the
// time when both cases were ready, so one pass could succeed by luck; 1000
// passes cannot.
const awaitObservationRepeats = 1000

// TestAwaitObservationHonorsAnAnswerAlreadyWaiting pins ObserveLivenessBounded's
// documented rule: a provider result that has arrived is honored even when the
// bound expired at the same moment. Both select cases are ready on every call
// (the answer sits in the buffered channel, the context is already done), with
// no goroutine and no clock, so the old random choice cannot pass by luck.
func TestAwaitObservationHonorsAnAnswerAlreadyWaiting(t *testing.T) {
	cases := []struct {
		name       string
		answer     observation
		wantStatus ObservationStatus
	}{
		{
			name:       "plain answer is complete",
			answer:     observation{liveness: Liveness{Running: true, Alive: true}},
			wantStatus: ObservationComplete,
		},
		{
			name: "ErrRuntimeUnavailable answer is incomplete with the provider's liveness preserved",
			answer: observation{
				liveness: Liveness{Running: true},
				err:      fmt.Errorf("probe: %w", ErrRuntimeUnavailable),
			},
			wantStatus: ObservationIncomplete,
		},
		{
			name: "other error answer stays complete",
			answer: observation{
				liveness: Liveness{Running: true},
				err:      errLivenessUnavailableForTest,
			},
			wantStatus: ObservationComplete,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			for i := 0; i < awaitObservationRepeats; i++ {
				results := make(chan observation, 1)
				results <- tc.answer

				got, status, err := awaitObservation(ctx, results)
				if got != tc.answer.liveness {
					t.Fatalf("pass %d: Liveness = %#v, want the waiting answer %#v", i, got, tc.answer.liveness)
				}
				if status != tc.wantStatus {
					t.Fatalf("pass %d: ObservationStatus = %v, want %v", i, status, tc.wantStatus)
				}
				if !errors.Is(err, tc.answer.err) && tc.answer.err != nil {
					t.Fatalf("pass %d: error = %v, want errors.Is(err, %v)", i, err, tc.answer.err)
				}
				if tc.answer.err == nil && err != nil {
					t.Fatalf("pass %d: error = %v, want nil (the context error must not replace a real answer)", i, err)
				}
			}
		})
	}
}

// TestAwaitObservationWithNothingWaitingReportsTheContextError keeps the
// other half of the contract: when no answer has arrived the bound's error is
// returned with a zero Liveness and ObservationIncomplete.
func TestAwaitObservationWithNothingWaitingReportsTheContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, status, err := awaitObservation(ctx, make(chan observation, 1))
	if status != ObservationIncomplete {
		t.Fatalf("ObservationStatus = %v, want ObservationIncomplete", status)
	}
	if got != (Liveness{}) {
		t.Fatalf("Liveness = %#v, want zero value when no answer arrived", got)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want errors.Is(err, context.Canceled)", err)
	}
}
