package coordclass

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func strp(s string) *string { return &s }

// workHolds is what a work store holds on a city that serves every
// infrastructure class from a binding; infraHolds is what that binding holds.
var (
	workHolds  = []Class{ClassWork}
	infraHolds = []Class{ClassGraph, ClassMessaging, ClassSessions, ClassOrders, ClassNudges}
)

// TestCheckFlipRefusesAWriteThatLeavesTheStoresClasses is the guard's table:
// every way the audit flipped a work bead in place, the reverse flip inside a
// binding, and the writes that must stay allowed.
func TestCheckFlipRefusesAWriteThatLeavesTheStoresClasses(t *testing.T) {
	task := beads.Bead{ID: "demo-1", Type: "task"}
	graphStep := beads.Bead{ID: "gcg-1", Type: "task", Metadata: map[string]string{"gc.root_bead_id": "gcg-0"}}
	syntheticConvoy := beads.Bead{ID: "demo-2", Type: "convoy", Metadata: map[string]string{"gc.synthetic": "true", "gc.root_bead_id": "gcg-0"}}
	stranded := beads.Bead{ID: "demo-3", Type: "task", Labels: []string{"gc:nudge"}}

	cases := []struct {
		name   string
		before beads.Bead
		m      Mutation
		holds  []Class
		want   Class // the refused After class; ClassWork with refuse=false means allowed
		refuse bool
	}{
		{"metadata gc.root_bead_id", task, Mutation{SetMetadata: map[string]string{"gc.root_bead_id": "gcg-1"}}, workHolds, ClassGraph, true},
		{"metadata gc.kind=wisp", task, Mutation{SetMetadata: map[string]string{"gc.kind": "wisp"}}, workHolds, ClassGraph, true},
		{"metadata gc.kind=workflow", task, Mutation{SetMetadata: map[string]string{"gc.kind": "workflow"}}, workHolds, ClassGraph, true},
		{"metadata gc.formula_contract=graph.v2", task, Mutation{SetMetadata: map[string]string{"gc.formula_contract": "graph.v2"}}, workHolds, ClassGraph, true},
		{"label wisp", task, Mutation{AddLabels: []string{"wisp"}}, workHolds, ClassGraph, true},
		{"label gc:wisp", task, Mutation{AddLabels: []string{"gc:wisp"}}, workHolds, ClassGraph, true},
		{"label gc:session", task, Mutation{AddLabels: []string{"gc:session"}}, workHolds, ClassSessions, true},
		{"label gc:wait", task, Mutation{AddLabels: []string{"gc:wait"}}, workHolds, ClassSessions, true},
		{"label gc:rt", task, Mutation{AddLabels: []string{"gc:rt"}}, workHolds, ClassSessions, true},
		{"label gc:fact", task, Mutation{AddLabels: []string{"gc:fact"}}, workHolds, ClassSessions, true},
		{"label gc:op", task, Mutation{AddLabels: []string{"gc:op"}}, workHolds, ClassSessions, true},
		{"label gc:nudge", task, Mutation{AddLabels: []string{"gc:nudge"}}, workHolds, ClassNudges, true},
		{"label order-tracking", task, Mutation{AddLabels: []string{"order-tracking"}}, workHolds, ClassOrders, true},
		{"label gc:extmsg-binding", task, Mutation{AddLabels: []string{"gc:extmsg-binding"}}, workHolds, ClassMessaging, true},
		{"set-labels to a session", task, Mutation{SetLabels: []string{"gc:session"}, ReplaceLabels: true}, workHolds, ClassSessions, true},
		{"type message", task, Mutation{Type: strp("message")}, workHolds, ClassMessaging, true},
		{"type session", task, Mutation{Type: strp("session")}, workHolds, ClassSessions, true},
		{"type wisp", task, Mutation{Type: strp("wisp")}, workHolds, ClassGraph, true},
		{"type convergence", task, Mutation{Type: strp("convergence")}, workHolds, ClassGraph, true},
		{"type op", task, Mutation{Type: strp("op")}, workHolds, ClassSessions, true},
		// A removal flips too: a synthetic convoy is work only while it is synthetic.
		{"unset gc.synthetic on a graph-marked convoy", syntheticConvoy, Mutation{UnsetMetadata: []string{"gc.synthetic"}}, workHolds, ClassGraph, true},
		{"blank gc.root_bead_id is not a graph marker", task, Mutation{SetMetadata: map[string]string{"gc.root_bead_id": "  "}}, workHolds, ClassWork, false},

		// The reverse, inside a binding.
		{"binding: clear the only graph marker", graphStep, Mutation{UnsetMetadata: []string{"gc.root_bead_id"}}, infraHolds, ClassWork, true},
		{"binding: blank the only graph marker", graphStep, Mutation{SetMetadata: map[string]string{"gc.root_bead_id": ""}}, infraHolds, ClassWork, true},
		{"binding: graph to sessions stays in the binding", graphStep, Mutation{AddLabels: []string{"gc:session"}}, infraHolds, ClassSessions, false},
		{"binding: ordinary metadata", graphStep, Mutation{SetMetadata: map[string]string{"gc.outcome": "pass"}}, infraHolds, ClassGraph, false},

		// Allowed in the work store.
		{"ordinary label", task, Mutation{AddLabels: []string{"triage"}}, workHolds, ClassWork, false},
		{"ordinary metadata", task, Mutation{SetMetadata: map[string]string{"gc.outcome": "pass"}}, workHolds, ClassWork, false},
		{"type bug", task, Mutation{Type: strp("bug")}, workHolds, ClassWork, false},
		{"convoy type stays work", task, Mutation{Type: strp("convoy")}, workHolds, ClassWork, false},
		// A stranded bead may be written without moving class, and may be
		// repaired back into the class its store holds.
		{"stranded: unrelated write", stranded, Mutation{SetMetadata: map[string]string{"note": "x"}}, workHolds, ClassNudges, false},
		{"stranded: repair to work", stranded, Mutation{RemoveLabels: []string{"gc:nudge"}}, workHolds, ClassWork, false},

		// A store that holds every class — a single-store city — never refuses.
		{"single store: nil holds", task, Mutation{AddLabels: []string{"gc:session"}}, nil, ClassSessions, false},
		{"single store: every class", task, Mutation{AddLabels: []string{"wisp"}}, Classes(), ClassGraph, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			after := c.m.Apply(c.before)
			if got := Classify(after); got != c.want {
				t.Fatalf("Classify(after) = %s, want %s (after=%+v)", got, c.want, after)
			}
			flip := CheckFlip(c.before, after, c.holds)
			if c.refuse != (flip != nil) {
				t.Fatalf("CheckFlip refused=%v, want %v (flip=%v)", flip != nil, c.refuse, flip)
			}
			if flip == nil {
				return
			}
			if flip.Before != Classify(c.before) || flip.After != c.want {
				t.Errorf("flip = %s -> %s, want %s -> %s", flip.Before, flip.After, Classify(c.before), c.want)
			}
			msg := flip.Error()
			for _, want := range []string{flip.Before.String(), flip.After.String()} {
				if !strings.Contains(msg, want) {
					t.Errorf("flip message %q does not name class %q", msg, want)
				}
			}
			if cmd := MintCommand(flip.After); cmd == "" || !strings.Contains(flip.Guidance(), cmd) {
				t.Errorf("guidance %q does not name the command %q that makes a %s bead", flip.Guidance(), cmd, flip.After)
			}
		})
	}
}

// TestMutationApplyDoesNotAliasTheBefore pins that computing the after shape
// never rewrites the caller's bead: the before has to stay the before.
func TestMutationApplyDoesNotAliasTheBefore(t *testing.T) {
	before := beads.Bead{Type: "task", Labels: []string{"a", "b"}, Metadata: map[string]string{"k": "v"}}
	m := Mutation{
		Type:          strp("bug"),
		AddLabels:     []string{"c"},
		RemoveLabels:  []string{"a"},
		SetMetadata:   map[string]string{"k2": "v2"},
		UnsetMetadata: []string{"k"},
	}
	after := m.Apply(before)
	if before.Type != "task" || len(before.Labels) != 2 || before.Labels[0] != "a" || before.Metadata["k"] != "v" || len(before.Metadata) != 1 {
		t.Fatalf("Apply mutated the before: %+v", before)
	}
	if after.Type != "bug" || strings.Join(after.Labels, ",") != "b,c" || after.Metadata["k2"] != "v2" || after.Metadata["k"] != "" {
		t.Fatalf("after = %+v", after)
	}
	if !m.TouchesClass() || (Mutation{}).TouchesClass() {
		t.Fatalf("TouchesClass: %v for a type write, %v for an empty one", m.TouchesClass(), (Mutation{}).TouchesClass())
	}
}

// TestTouchesClassIsExact checks the vocabulary table that both Classify and
// TouchesClass read (ClassifierLabels, ClassifierLabelPrefixes,
// ClassifierMetadataKeys) from both sides, by enumerating it:
//
//   - every entry is LIVE: for each label, prefix and key there is a bead in a
//     pool built from the table whose class an add/remove (set/unset) of that
//     entry changes, so the table carries nothing TouchesClass would read a
//     needless before-read for;
//   - nothing outside it is: Classify of a bead equals Classify of the bead
//     with every non-table label and key stripped, over the same pool with
//     non-table labels and keys mixed in. Classify reads labels and metadata
//     only through the table (hasLabel, hasLabelPrefix, classMeta), so this
//     holds by construction; the test pins the construction.
//
// It cannot prove a FUTURE arm is reached through the table — the
// construction does that (an arm that reads around it never fires and the
// golden table fails) — and it does not enumerate types, which TouchesClass
// always treats as live.
func TestTouchesClassIsExact(t *testing.T) {
	labels := ClassifierLabels()
	for _, p := range ClassifierLabelPrefixes() {
		labels = append(labels, p+"binding")
	}
	keys := ClassifierMetadataKeys()
	// "binding" is the session_id that makes the session: prefix probe label
	// (session:binding) name a wait's own session.
	values := []string{"", "wisp", "workflow", "graph.v2", "gcg-1", "true", "binding"}
	types := []string{"task", "convoy", "molecule", "message", "session", "op", "convergence", "gate", "wisp"}
	junkLabels := []string{"triage", "Session:gc-7", "gc:rt-conv:gc-7", "gc:op-conv:gc-7", "order-run:x", "provides:cap", "gc:other", "Wisp", "gc:extmsg", "wisp "}
	junkKeys := []string{"gc.outcome", "gc.output_json", "gc.session_id", "gc.routed_to", "molecule_id", "gc.step_id", "GC.KIND"}

	var pool []beads.Bead
	for _, typ := range types {
		pool = append(pool, beads.Bead{Type: typ})
		for _, l := range labels {
			pool = append(pool, beads.Bead{Type: typ, Labels: []string{l}})
			pool = append(pool, beads.Bead{Type: typ, Labels: []string{l, "order-tracking"}})
		}
		for _, k := range keys {
			for _, v := range values {
				pool = append(pool, beads.Bead{Type: typ, Metadata: map[string]string{k: v}})
				pool = append(pool, beads.Bead{Type: typ, Labels: []string{"order-tracking", "wisp"}, Metadata: map[string]string{k: v}})
				pool = append(pool, beads.Bead{Type: typ, Metadata: map[string]string{k: v, "gc.synthetic": "true", "gc.root_bead_id": "gcg-1"}})
			}
		}
	}

	live := func(write func(beads.Bead) beads.Bead) bool {
		for _, b := range pool {
			if Classify(b) != Classify(write(b)) {
				return true
			}
		}
		return false
	}
	for _, l := range labels {
		l := l
		add := func(b beads.Bead) beads.Bead { return Mutation{AddLabels: []string{l}}.Apply(b) }
		remove := func(b beads.Bead) beads.Bead { return Mutation{RemoveLabels: []string{l}}.Apply(b) }
		if !live(add) && !live(remove) {
			t.Errorf("classifier label %q never changes a class: drop it from the table", l)
		}
		if !(Mutation{AddLabels: []string{l}}).TouchesClass() {
			t.Errorf("TouchesClass calls classifier label %q inert", l)
		}
	}
	for _, k := range keys {
		k := k
		found := live(func(b beads.Bead) beads.Bead { return Mutation{UnsetMetadata: []string{k}}.Apply(b) })
		for _, v := range values {
			v := v
			found = found || live(func(b beads.Bead) beads.Bead { return Mutation{SetMetadata: map[string]string{k: v}}.Apply(b) })
		}
		if !found {
			t.Errorf("classifier key %q never changes a class: drop it from the table", k)
		}
		if !(Mutation{UnsetMetadata: []string{k}}).TouchesClass() {
			t.Errorf("TouchesClass calls classifier key %q inert", k)
		}
	}

	for _, b := range pool {
		noisy := b
		noisy.Labels = append(append([]string(nil), b.Labels...), junkLabels...)
		noisy.Metadata = map[string]string{}
		for k, v := range b.Metadata {
			noisy.Metadata[k] = v
		}
		for _, k := range junkKeys {
			noisy.Metadata[k] = "wisp"
		}
		if Classify(noisy) != Classify(b) {
			t.Fatalf("labels/keys outside the table changed the class of %+v: %s vs %s", b, Classify(noisy), Classify(b))
		}
	}
	for _, l := range junkLabels {
		if (Mutation{AddLabels: []string{l}}).TouchesClass() {
			t.Errorf("TouchesClass calls non-classifier label %q live", l)
		}
	}
	for _, k := range junkKeys {
		if (Mutation{SetMetadata: map[string]string{k: "x"}}).TouchesClass() {
			t.Errorf("TouchesClass calls non-classifier key %q live", k)
		}
	}
}

// TestOwnSessionWaitWritesAreClassWrites pins the own-session-wait arm to the
// vocabulary table from the write side. The arm reads a wait's session:<id>
// label and its session_id key, so a write of either can move the bead between
// work and sessions: TouchesClass must call each such write live, and CheckFlip
// must refuse it in a store that does not hold the new class. An arm that read
// either around the table would still classify every bead correctly, and
// TestTouchesClassIsExact would pass, while TouchesClass let the write skip the
// before-read the flip guard needs.
func TestOwnSessionWaitWritesAreClassWrites(t *testing.T) {
	for _, typ := range []string{"gate", "wait"} {
		labeled := beads.Bead{ID: "demo-1", Type: typ, Labels: []string{"session:s-1"}}
		keyed := beads.Bead{ID: "demo-2", Type: typ, Metadata: map[string]string{"session_id": "s-1"}}
		own := beads.Bead{ID: "gcs-1", Type: typ, Labels: []string{"session:s-1"}, Metadata: map[string]string{"session_id": "s-1"}}
		cases := []struct {
			name   string
			before beads.Bead
			m      Mutation
			holds  []Class
			want   Class
		}{
			{"add the label naming its own session", keyed, Mutation{AddLabels: []string{"session:s-1"}}, workHolds, ClassSessions},
			{"set session_id to the session its label names", labeled, Mutation{SetMetadata: map[string]string{"session_id": "s-1"}}, workHolds, ClassSessions},
			{"binding: remove the label", own, Mutation{RemoveLabels: []string{"session:s-1"}}, infraHolds, ClassWork},
			{"binding: point session_id at another session", own, Mutation{SetMetadata: map[string]string{"session_id": "s-2"}}, infraHolds, ClassWork},
			{"binding: unset session_id", own, Mutation{UnsetMetadata: []string{"session_id"}}, infraHolds, ClassWork},
		}
		for _, c := range cases {
			t.Run(typ+": "+c.name, func(t *testing.T) {
				after := c.m.Apply(c.before)
				if got := Classify(c.before); got == c.want {
					t.Fatalf("Classify(before) = %s already, so the write moves nothing (before=%+v)", got, c.before)
				}
				if got := Classify(after); got != c.want {
					t.Fatalf("Classify(after) = %s, want %s (after=%+v)", got, c.want, after)
				}
				if !c.m.TouchesClass() {
					t.Errorf("TouchesClass() = false for %+v, which moves a %s bead from %s to %s: a caller would skip the before-read CheckFlip needs", c.m, typ, Classify(c.before), c.want)
				}
				if flip := CheckFlip(c.before, after, c.holds); flip == nil || flip.After != c.want {
					t.Errorf("CheckFlip = %v, want a refused flip to %s", flip, c.want)
				}
			})
		}
	}
}

// TestMintCommandNamesEveryClass keeps a refusal from ever pointing at nothing.
func TestMintCommandNamesEveryClass(t *testing.T) {
	for _, c := range Classes() {
		if MintCommand(c) == "" {
			t.Errorf("MintCommand(%s) is empty", c)
		}
	}
}
