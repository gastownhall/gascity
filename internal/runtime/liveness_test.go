package runtime

import (
	"context"
	"testing"
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
