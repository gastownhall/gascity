package runproj

import "testing"

// Regression tests: a pending node whose upstream has not finished is WAITING
// ITS TURN — it stays "pending" (neutral) rather than being promoted to
// "blocked" (the operator-attention alarm). "blocked" is reserved for nodes the
// store itself marks blocked.

func TestDisplayStatusDepWaitingStaysPending(t *testing.T) {
	nodes := []RunDisplayNode{
		{ID: "a", Status: "active"},
		{ID: "b", Status: "pending"},
	}
	edges := []RunDisplayEdge{{From: "a", To: "b", Kind: "blocks"}}
	out := applyDisplayNodeStates(nodes, edges)
	if got := statusOf(t, out, "b"); got != "pending" {
		t.Fatalf("dep-waiting node status = %q, want %q", got, "pending")
	}
}

func TestDisplayStatusDepsMetPromotesToReady(t *testing.T) {
	nodes := []RunDisplayNode{
		{ID: "a", Status: "completed"},
		{ID: "b", Status: "pending"},
	}
	edges := []RunDisplayEdge{{From: "a", To: "b", Kind: "blocks"}}
	out := applyDisplayNodeStates(nodes, edges)
	if got := statusOf(t, out, "b"); got != "ready" {
		t.Fatalf("deps-met node status = %q, want %q", got, "ready")
	}
}

func TestDisplayStatusStoreBlockedIsPreserved(t *testing.T) {
	nodes := []RunDisplayNode{
		{ID: "a", Status: "active"},
		{ID: "b", Status: "blocked"},
	}
	edges := []RunDisplayEdge{{From: "a", To: "b", Kind: "blocks"}}
	out := applyDisplayNodeStates(nodes, edges)
	if got := statusOf(t, out, "b"); got != "blocked" {
		t.Fatalf("store-blocked node status = %q, want %q", got, "blocked")
	}
}

func statusOf(t *testing.T, nodes []RunDisplayNode, id string) string {
	t.Helper()
	for _, n := range nodes {
		if n.ID == id {
			return n.Status
		}
	}
	t.Fatalf("node %q missing", id)
	return ""
}
