package herdr

import (
	"errors"
	"fmt"
	"testing"
)

// wrapTaken builds an error shaped exactly like client.run's output for a
// herdr agent_name_taken rejection: the typed *herdrError wrapped with %w
// under an outer context string.
func wrapTaken() error {
	return fmt.Errorf("herdr [agent start x]: %w", &herdrError{
		Code:    "agent_name_taken",
		Message: `agent name x is already used; candidates: pane_id=w3F:pW status=Unknown`,
	})
}

func TestHerdrErrorCodeExtractsWrappedCode(t *testing.T) {
	if got := herdrErrorCode(wrapTaken()); got != "agent_name_taken" {
		t.Errorf("herdrErrorCode = %q; want agent_name_taken", got)
	}
	if got := herdrErrorCode(errors.New("plain transport failure")); got != "" {
		t.Errorf("herdrErrorCode(plain) = %q; want empty", got)
	}
	if got := herdrErrorCode(nil); got != "" {
		t.Errorf("herdrErrorCode(nil) = %q; want empty", got)
	}
}

// A successful start passes straight through, untouched and unadopted.
func TestResolveAgentNameTakenSuccessPassesThrough(t *testing.T) {
	want := agentInfo{Name: "x", PaneID: "w1:pA"}
	got, adopted, err := resolveAgentNameTaken(want, nil, agentStartOps{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != want {
		t.Errorf("got %+v; want %+v", got, want)
	}
	if adopted {
		t.Error("adopted=true for a fresh successful start; want false")
	}
}

// A non-taken error is surfaced verbatim — recovery must not swallow real
// failures (e.g. openpty tab_create_failed, transport errors).
func TestResolveAgentNameTakenNonTakenErrorSurfaces(t *testing.T) {
	boom := errors.New("herdr [agent start x]: some_other_failure: nope")
	called := false
	_, adopted, err := resolveAgentNameTaken(agentInfo{}, boom, agentStartOps{
		getAgent: func() (agentInfo, bool, error) { called = true; return agentInfo{}, false, nil },
	})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v; want the original non-taken error", err)
	}
	if adopted {
		t.Error("adopted=true on a non-taken error; want false")
	}
	if called {
		t.Error("getAgent was called for a non-taken error; recovery must not engage")
	}
}

// agent_name_taken + the holder's process is alive → adopt it: return the
// existing agent with adopted=true, do NOT reap, do NOT retry. This is the
// storm-breaker, and adopted=true tells Start to skip re-priming a live agent.
func TestResolveAgentNameTakenAdoptsLiveHolder(t *testing.T) {
	existing := agentInfo{Name: "x", PaneID: "w3F:pW", TabID: "w3F:tE"}
	reaped, retried := false, false
	got, adopted, err := resolveAgentNameTaken(agentInfo{}, wrapTaken(), agentStartOps{
		getAgent:   func() (agentInfo, bool, error) { return existing, true, nil },
		paneAlive:  func(paneID string) bool { return paneID == "w3F:pW" },
		closePane:  func(string) error { reaped = true; return nil },
		retryStart: func() (agentInfo, error) { retried = true; return agentInfo{}, nil },
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != existing {
		t.Errorf("got %+v; want adopted existing %+v", got, existing)
	}
	if !adopted {
		t.Error("adopted=false for a live holder; want true so Start skips re-delivery")
	}
	if reaped {
		t.Error("closePane called on a live holder; must adopt, not reap")
	}
	if retried {
		t.Error("retryStart called on a live holder; must adopt, not retry")
	}
}

// agent_name_taken + the holder is a stale/dead pane → reap it, then start
// once more (bounded: exactly one retry, no loop). A retried start is a fresh
// agent, not an adoption, so adopted=false (Start still primes it).
func TestResolveAgentNameTakenReapsStaleThenRetries(t *testing.T) {
	stale := agentInfo{Name: "x", PaneID: "w3F:pOLD"}
	fresh := agentInfo{Name: "x", PaneID: "w3F:pNEW"}
	var reapedPane string
	retries := 0
	got, adopted, err := resolveAgentNameTaken(agentInfo{}, wrapTaken(), agentStartOps{
		getAgent:   func() (agentInfo, bool, error) { return stale, true, nil },
		paneAlive:  func(string) bool { return false },
		closePane:  func(paneID string) error { reapedPane = paneID; return nil },
		retryStart: func() (agentInfo, error) { retries++; return fresh, nil },
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if reapedPane != "w3F:pOLD" {
		t.Errorf("reaped %q; want the stale holder pane w3F:pOLD", reapedPane)
	}
	if retries != 1 {
		t.Errorf("retryStart called %d times; want exactly 1 (bounded, no loop)", retries)
	}
	if got != fresh {
		t.Errorf("got %+v; want fresh start %+v", got, fresh)
	}
	if adopted {
		t.Error("adopted=true after reap+retry; want false (fresh start, not adoption)")
	}
}

// agent_name_taken but the holder can't be inspected (getAgent errors or
// reports absent) → surface the original error rather than guessing.
func TestResolveAgentNameTakenUninspectableHolderSurfacesOriginal(t *testing.T) {
	orig := wrapTaken()
	_, adopted, err := resolveAgentNameTaken(agentInfo{}, orig, agentStartOps{
		getAgent: func() (agentInfo, bool, error) { return agentInfo{}, false, nil },
	})
	if !errors.Is(err, orig) {
		t.Errorf("err = %v; want the original agent_name_taken error when the holder is uninspectable", err)
	}
	if adopted {
		t.Error("adopted=true when the holder is uninspectable; want false")
	}
}
