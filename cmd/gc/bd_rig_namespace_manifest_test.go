package main

// Tests of gc bd reading flags the way pflag does, against each verb's
// manifest (hidden value flags, "--" given as a value, the claim actor and
// claim values), and of deny-all legs.

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// bd has value flags its manifests did not list; pflag hands such a flag
// the next token, so a help token after one is a value and bd runs the
// command. Help counts only on an argv that reads cleanly.
func TestBdRigNamespaceHelpAfterHiddenValueFlag(t *testing.T) {
	c := brnCheck(t, "mayor", brnOwned, brnForeign)
	for _, args := range [][]string{
		{"close", "bt-2", "--resolution", "--help"},
		{"close", "bt-2", "-m", "--help"},
		{"close", "bt-2", "--message", "--help"},
		{"close", "bt-2", "--comment", "--help"},
		{"update", "bt-2", "--assignee", "mayor", "--body", "--help"},
		{"update", "bt-2", "--assignee", "mayor", "-m", "--help"},
		{"update", "bt-2", "--assignee", "mayor", "--message", "--help"},
		{"update", "bt-2", "--assignee", "mayor", "--description-file", "--help"},
		{"update", "bt-2", "--assignee", "mayor", "--acceptance-criteria", "--help"},
		{"close", "bt-2", "--not-a-known-flag", "--help"},
		{"close", "bt-2", "--help=maybe"},
	} {
		if _, _, refused := c.check(args); !refused {
			t.Errorf("gc bd %v passes; bd runs it", args)
		}
	}
	c.actor = "beta/crew.worker"
	for _, args := range [][]string{
		{"close", "bt-1", "--resolution", "done"},
		{"close", "bt-1", "-m", "done"},
		{"close", "bt-1", "--message=done"},
		{"close", "bt-1", "--comment", "done"},
		{"update", "bt-1", "--body", "text"},
		{"update", "bt-1", "-m", "text"},
		{"update", "bt-1", "--message", "text"},
		{"update", "bt-1", "--description-file", "d.md"},
		{"update", "bt-1", "--acceptance-criteria", "x"},
		{"note", "bt-1", "--file", "n.md"},
		{"note", "bt-1", "--stdin"},
		{"comment", "bt-1", "--stdin"},
		{"close", "bt-2", "--resolution", "x", "--help"}, // help, really on
	} {
		if _, msg, refused := c.check(args); refused {
			t.Errorf("gc bd %v refused: %q", args, msg)
		}
	}
}

// A literal "--" given as a flag's value terminates nothing: pflag reads the
// flags after it.
func TestBdRigNamespaceDashDashAsValueHidesNothing(t *testing.T) {
	c := brnCheck(t, "beta/crew.worker", brnOwned)
	for _, args := range [][]string{
		{"delete", "bt-1", "--actor", "--", "--cascade"},
		{"delete", "bt-1", "--actor", "--", "--from-file", "ids.txt"},
		{"create", "--title", "--", "--file", "plan.md"},
		{"create", "--title", "--", "--graph", "g.json"},
		{"create", "--title", "--", "--deps", "blocks:bt-2"},
		{"close", "bt-1", "--reason", "--", "--db", "/other"},
		{"update", "bt-1", "--notes", "--", "--assignee", "mayor"},
		{"update", "bt-1", "--append-notes", "--", "--set-metadata", "gc.owner_namespace=alpha/"},
		{"create", "--title", "--", "--assignee", "mayor"},
		{"create", "--title", "--", "--metadata", `{}`, "--metadata", `{"k":"v"}`},
	} {
		if _, _, refused := c.check(args); !refused {
			t.Errorf("gc bd %v passes", args)
		}
	}
	// A real terminator still ends the flags.
	if _, msg, refused := c.check([]string{"create", "--title", "x", "--", "--file"}); refused {
		t.Errorf("create with a positional after a real -- refused: %q", msg)
	}
}

// The claim actor is the --actor bd uses: the last one, outside value
// positions.
func TestBdRigNamespaceClaimActorIsTheOneBdUses(t *testing.T) {
	c := brnCheck(t, "beta/crew.worker", brnOwned)
	for _, args := range [][]string{
		{"update", "bt-1", "--claim", "--actor", "beta/crew.worker", "--actor", "mayor"},
		{"--actor", "beta/crew.worker", "update", "bt-1", "--claim", "--actor=mayor"},
	} {
		if _, _, refused := c.check(args); !refused {
			t.Errorf("gc bd %v passes; bd claims as mayor", args)
		}
	}
	c.actor = "mayor"
	if _, _, refused := c.check([]string{"update", "bt-1", "--claim", "--append-notes", "--actor=beta/crew.worker"}); !refused {
		t.Error("an --actor in a value position was taken as the claim actor")
	}
	if _, msg, refused := c.check([]string{"update", "bt-1", "--claim", "--actor", "mayor", "--actor", "beta/crew.worker"}); refused {
		t.Errorf("last --actor admitted, refused: %q", msg)
	}
}

// Claim flags fail closed on a value ParseBool rejects.
func TestBdRigNamespaceUnparsableClaimValues(t *testing.T) {
	c := brnCheck(t, "mayor", brnOwned)
	for _, args := range [][]string{
		{"update", "bt-1", "--claim=maybe"},
		{"close", "bt-1", "--continue", "--no-auto=nope"},
		{"close", "bt-1", "--continue=maybe", "--no-auto=nope"},
	} {
		if _, _, refused := c.check(args); !refused {
			t.Errorf("gc bd %v passes", args)
		}
	}
	if _, msg, refused := c.check([]string{"update", "bt-1", "--claim=false", "--status", "open"}); refused {
		t.Errorf("--claim=false refused: %q", msg)
	}
}

// A deny-all leg stays deny-all for a row whose prefix maps nowhere, and a
// deny-all candidate keeps its reason when the leg's names are combined.
func TestHookRigGuardDenyAllLegStaysDenyAll(t *testing.T) {
	g := hrgGuard(t)
	deny := denyAllRigNamespaceBecause(g.sessions, errors.New("its bead store is rig \"alpha\"'s"))
	if ns := rigNamespaceForLeg(deny, g.cfg, "or-77", g.sessions); ns == nil || ns.namespace() != "" {
		t.Fatalf("deny-all leg, unmapped prefix: ns = %+v, want deny-all", ns)
	}
	bound := g.forLeg("beta", "")
	bound.leg = deny
	if kept := bound.filterCandidates([]beads.Bead{{ID: "or-77", Status: "open", Metadata: rnsMD("gc.routed_to", "beta/crew.worker")}}, io.Discard); len(kept) != 0 {
		t.Fatalf("a deny-all leg kept an unmapped-prefix row: %v", kept)
	}
	beta := g.forLeg("beta", "").leg
	if beta == nil || beta.rigName() != "beta" {
		t.Fatalf("fixture: beta leg = %+v", beta)
	}
	got := agreeingRigNamespace([]*rigNamespace{deny, beta}, g.sessions)
	if got.namespace() != "" || !strings.Contains(got.denyReason, `rig "alpha"`) {
		t.Fatalf("combined leg = %+v, want the deny-all with its own reason", got)
	}
}
