package session

import (
	"slices"
	"testing"
)

// Kills Facts drifting from the registry: it holds every premise key and
// then the assignment identity keys outside the premise, no lease or other
// key, and every exception is a premise key.
func TestFactsKeys(t *testing.T) {
	if len(factKeys) != numFactKeys {
		t.Fatalf("Facts holds %d keys, the registry classes %d: set numFactKeys", numFactKeys, len(factKeys))
	}
	for _, f := range fields {
		at := slices.Index(factKeys, f.Key)
		switch {
		case f.InPremise() && (at < 0 || at >= identityFrom):
			t.Errorf("premise key %s at %d, want before %d", f.Key, at, identityFrom)
		case !f.InPremise() && f.Class == ClassAssignmentIdentity && at < identityFrom:
			t.Errorf("assignment identity key %s at %d, want from %d", f.Key, at, identityFrom)
		case !f.InPremise() && f.Class != ClassAssignmentIdentity && at >= 0:
			t.Errorf("key %s (class %d) is in Facts", f.Key, f.Class)
		}
	}
	for site, e := range factsSites {
		if e.Reason == "" || len(e.Drop) > 0 == (len(e.Only) > 0) {
			t.Errorf("%s: want a reason, and either keys to drop or the only keys compared", site)
		}
		for _, k := range append(slices.Clone(e.Drop), e.Only...) {
			if i := slices.Index(factKeys, k); i < 0 || i >= identityFrom {
				t.Errorf("%s: %s is not a premise key", site, k)
			}
		}
	}
}

// Kills a Facts comparison that sees too much or too little: a premise key
// moves it, an advisory or lease key does not; Premise drops the assignment
// identity keys; FactsAfterStart drops exactly its keys; FactsCommit
// compares only the token and wants an S2 state.
func TestFactsCompare(t *testing.T) {
	row := map[string]string{"state": "creating", "generation": "1", "alias": "a", "session_key": "k", "instance_token": "tok"}
	with := func(kv ...string) Facts {
		p := MetadataPatch{}
		for i := 0; i+1 < len(kv); i += 2 {
			p[kv[i]] = kv[i+1]
		}
		return FactsOf(p.Apply(row))
	}
	base := FactsOf(row)
	for _, c := range []struct {
		key  string
		same bool
	}{{"state", false}, {"wake_attempts", false}, {"synced_at", true}, {RuntimeLeaseEpochKey, true}} {
		if got := FactsDefault.Match(with(c.key, "x"), base); got != c.same {
			t.Errorf("%s moved: match %t, want %t", c.key, got, c.same)
		}
	}
	if with("alias", "b") == base || with("alias", "b").Premise() != base.Premise() {
		t.Error("an alias change: want Facts to differ and their Premise to match")
	}
	for _, c := range []struct {
		site  FactsSite
		kv    []string
		match bool
	}{
		{FactsAfterStart, []string{"session_key", "k2", "current_claim_bead_id", "b"}, true},
		{FactsAfterStart, []string{"detached_at", "x"}, false},
		{FactsAfterStart, []string{"state", "awake"}, false},
		{FactsCommit, []string{"held_until", "x", "state", "active"}, true},
		{FactsCommit, []string{"state", "suspended"}, false},
		{FactsCommit, []string{"instance_token", "other"}, false},
	} {
		if got := c.site.Match(with(c.kv...), base); got != c.match {
			t.Errorf("%s with %v: match %t, want %t", c.site, c.kv, got, c.match)
		}
	}
}
