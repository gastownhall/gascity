package opjournal

import (
	"errors"
	"testing"
	"time"
)

func TestRegistryHasTheTenKinds(t *testing.T) {
	want := []Kind{
		KindMessage, KindInterrupt, KindRespond, KindCreate, KindWake,
		KindSuspend, KindResume, KindRestart, KindKill, KindClose,
	}
	got := Kinds()
	if len(got) != 10 {
		t.Fatalf("len(Kinds()) = %d, want 10: %v", len(got), got)
	}
	for i, k := range want {
		if got[i] != k {
			t.Errorf("Kinds()[%d] = %q, want %q", i, got[i], k)
		}
	}
}

// TestRegistryTripwire keeps the kind set small and closed. Raising MaxKinds
// is a design decision, not a code change.
func TestRegistryTripwire(t *testing.T) {
	if MaxKinds != 12 {
		t.Fatalf("MaxKinds = %d; the cap is 12", MaxKinds)
	}
	if n := len(Kinds()); n > MaxKinds {
		t.Fatalf("%d kinds registered, the cap is %d", n, MaxKinds)
	}
}

func TestRegistryRowsAreComplete(t *testing.T) {
	seen := map[Kind]bool{}
	for _, k := range Kinds() {
		spec, err := Lookup(k)
		if err != nil {
			t.Fatalf("Lookup(%q): %v", k, err)
		}
		if seen[k] {
			t.Errorf("kind %q registered twice", k)
		}
		seen[k] = true
		if spec.Kind != k {
			t.Errorf("Lookup(%q).Kind = %q", k, spec.Kind)
		}
		wantLane := LaneConversation
		if spec.Class == ClassLifecycle {
			wantLane = LaneLifecycle
		}
		if spec.Class == "" || spec.Lane != wantLane {
			t.Errorf("%s: class %q lane %q, want lane %q", k, spec.Class, spec.Lane, wantLane)
		}
		if !spec.allowsScope(spec.DefaultScope) {
			t.Errorf("%s: default scope %q not in %v", k, spec.DefaultScope, spec.Scopes)
		}
		if spec.StartWithin <= 0 || spec.FinishWithin < spec.StartWithin {
			t.Errorf("%s: start %s finish %s", k, spec.StartWithin, spec.FinishWithin)
		}
		if spec.PayloadRequired && !spec.PayloadAllowed {
			t.Errorf("%s: payload required but not allowed", k)
		}
		if (k == KindMessage) != (len(spec.When) > 0) {
			t.Errorf("%s: When = %v; only message has delivery modes", k, spec.When)
		}
		if spec.OnConversationClose == "" {
			t.Errorf("%s: no conversation-close rule", k)
		}
		if (k == KindClose) != (spec.OnConversationClose == CloseRuleSucceed) {
			t.Errorf("%s: close rule %q", k, spec.OnConversationClose)
		}
		if spec.TurnScoped && k != KindInterrupt {
			t.Errorf("%s: only interrupt takes a turn", k)
		}
		if spec.InteractionScoped && k != KindRespond {
			t.Errorf("%s: only respond takes an interaction", k)
		}
		// The default policy passes the kind's own validation once defaulted.
		if err := validatePolicy(applyPolicyDefaults(Policy{}, spec), spec); err != nil {
			t.Errorf("%s: default policy invalid: %v", k, err)
		}
		if applyPolicyDefaults(Policy{}, spec) != spec.DefaultPolicy {
			t.Errorf("%s: defaulting a zero policy = %+v, want %+v", k, applyPolicyDefaults(Policy{}, spec), spec.DefaultPolicy)
		}
	}
}

func TestRegistryDeadlinesAndDefaults(t *testing.T) {
	cases := []struct {
		kind          Kind
		start, finish time.Duration
		scope         ScopeKind
	}{
		{KindMessage, 24 * time.Hour, 24*time.Hour + 10*time.Minute, ScopeConversation},
		{KindInterrupt, time.Minute, 3 * time.Minute, ScopeSession},
		{KindRespond, time.Hour, time.Hour + 5*time.Minute, ScopeSession},
		{KindCreate, 15 * time.Minute, time.Hour, ScopeConversation},
		{KindKill, 15 * time.Minute, time.Hour, ScopeSession},
		{KindRestart, 15 * time.Minute, time.Hour, ScopeConversation},
	}
	for _, c := range cases {
		spec, _ := Lookup(c.kind)
		if spec.StartWithin != c.start || spec.FinishWithin != c.finish || spec.DefaultScope != c.scope {
			t.Errorf("%s: (%s, %s, %s), want (%s, %s, %s)", c.kind, spec.StartWithin, spec.FinishWithin,
				spec.DefaultScope, c.start, c.finish, c.scope)
		}
	}
	msg, _ := Lookup(KindMessage)
	if msg.DefaultPolicy.Message != (MessagePolicy{When: WhenBoundary, Wake: WakeIfNeeded, Role: RolePrompt, OnUnsupported: FallbackBoundary}) ||
		msg.DefaultPolicy.Guarantee != GuaranteeAtLeastOnce {
		t.Errorf("message defaults = %+v", msg.DefaultPolicy)
	}
	restart, _ := Lookup(KindRestart)
	if restart.DefaultPolicy.Restart.Mode != RestartContinue || restart.DefaultPolicy.Guarantee != "" {
		t.Errorf("restart defaults = %+v", restart.DefaultPolicy)
	}
}

func TestLookupUnknownKind(t *testing.T) {
	if _, err := Lookup("teleport"); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("Lookup(teleport) = %v, want ErrUnknownKind", err)
	}
}

func TestLookupReturnsACopy(t *testing.T) {
	spec, _ := Lookup(KindMessage)
	spec.Scopes[0] = "mutated"
	again, _ := Lookup(KindMessage)
	if again.Scopes[0] != ScopeConversation {
		t.Fatalf("Lookup leaked the registry's slice")
	}
}
