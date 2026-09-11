package runtime

import (
	"context"
	"errors"
	"testing"
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
