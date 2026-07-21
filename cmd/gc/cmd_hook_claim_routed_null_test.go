package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// A gc.routed_to that leaked the literal JSON-null string "null" (a clear that
// did not clear — e.g. a raw `bd update --set-metadata gc.routed_to=null` or a
// serialization round-trip) must be treated as UNROUTED, not as a concrete route
// target named "null". Left as a literal target it matches no session and
// strands the bead unclaimable forever (a contributor to the zero-claim /
// starvation class). No legitimate route target is ever named "null".
func TestHookClaimRoutedToNullLiteralTreatedAsUnrouted(t *testing.T) {
	// Workflow root: unrouted work is claimable via its run target, so a "null"
	// routed_to must fall through to the run-target match.
	workflow := beads.Bead{
		ID: "b-workflow",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:  "null",
			beadmeta.KindMetadataKey:      beadmeta.KindWorkflow,
			beadmeta.RunTargetMetadataKey: "rig/worker",
		},
	}
	if !hookClaimMatchesRoute(workflow, []string{"rig/worker"}) {
		t.Fatalf(`routed_to="null" workflow-root should be treated as unrouted and match its run target "rig/worker", but did not`)
	}
	if got := hookClaimRoute(workflow); got != "rig/worker" {
		t.Fatalf(`hookClaimRoute with routed_to="null" workflow-root: got %q, want run target "rig/worker"`, got)
	}

	// Plain bead: routed_to="null" is simply unrouted (empty display route), and
	// must not falsely match a real route target.
	plain := beads.Bead{
		ID:       "b-plain",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "null"},
	}
	if got := hookClaimRoute(plain); got != "" {
		t.Fatalf(`hookClaimRoute with plain routed_to="null": got %q, want "" (unrouted)`, got)
	}
	if hookClaimMatchesRoute(plain, []string{"null"}) {
		t.Fatalf(`a bead with routed_to="null" must NOT match a route target literally named "null" — "null" means unrouted`)
	}

	// Regression guard: a genuine route still matches exactly as before.
	routed := beads.Bead{
		ID:       "b-routed",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"},
	}
	if !hookClaimMatchesRoute(routed, []string{"rig/worker"}) {
		t.Fatalf("a genuinely routed bead must still match its target")
	}
}
