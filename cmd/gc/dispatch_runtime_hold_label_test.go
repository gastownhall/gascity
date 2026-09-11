package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/config"
)

// This file expresses the ga-x9kptu / ga-5736js acceptance criteria for the
// shell-generated control-ready query (workflowServeControlReadyQueryForBeads):
// routed_ready() (route-scoped, unassigned) must exclude beads carrying a
// beadmeta.DispatchHoldLabels value, while assignee_ready() (Tier 1/2) stays
// hold-transparent. routed_ready() is a single shared function invoked once
// for the primary target and once each for the legacy and bare-route
// aliases, so fixing it once must apply uniformly across all three.
//
// TestWorkflowServeControlReadyQueryShellFallbackUnreachable closes the
// reachability question this bead's acceptance criteria raised: whether
// routed_ready()/assignee_ready() can ever actually run as a subprocess via
// nextWorkflowServeBeads's raw shellWorkQueryWithEnv fallback, or whether
// tryControlReadyFromCacheOrFallback always intercepts first. It proves the
// latter, so the string-level assertions above are defense-in-depth on
// currently-dead code, not coverage of a live production path -- the real
// enforcement for this query shape runs through evaluateControlReady /
// filterReadyByRoute (see dispatch_control_ready_hold_label_test.go).

func TestWorkflowServeControlReadyQueryRoutedReadyExcludesDispatchHoldLabels(t *testing.T) {
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"})
	for _, label := range beadmeta.DispatchHoldLabels {
		want := `--exclude-label "` + label + `"`
		if count := strings.Count(query, want); count != 2 {
			t.Errorf("workflowServeControlReadyQuery() contains %q %d times, want 2 (routed_ready's two bd-ready calls): %s", want, count, query)
		}
	}
}

func TestWorkflowServeControlReadyQueryAssigneeReadyDoesNotExcludeDispatchHoldLabels(t *testing.T) {
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"})
	start := strings.Index(query, `assignee_ready() { `)
	if start < 0 {
		t.Fatalf("workflowServeControlReadyQuery() missing assignee_ready() definition: %s", query)
	}
	relEnd := strings.Index(query[start:], `; }; `)
	if relEnd < 0 {
		t.Fatalf("workflowServeControlReadyQuery() could not locate end of assignee_ready() body: %s", query)
	}
	body := query[start : start+relEnd]
	if strings.Contains(body, "--exclude-label") {
		t.Errorf("assignee_ready() body = %q, must stay hold-transparent (Tier 1/2 assignee-scoped)", body)
	}
}

func TestWorkflowServeControlReadyQueryRoutedReadyAppliesToAllRouteAliases(t *testing.T) {
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"})
	if count := strings.Count(query, `routed_ready "`); count != 3 {
		t.Errorf("workflowServeControlReadyQuery() calls routed_ready %d times, want 3 (target, legacy, bare route): %s", count, query)
	}
}

// TestWorkflowServeControlReadyQueryShellFallbackUnreachable proves
// nextWorkflowServeBeads's raw shell fallback (shellWorkQueryWithEnv) can
// never execute a control-ready-shaped query. tryControlReadyFromCacheOrFallback
// returns handled=false only when parseControlReadyQuery fails to recognize
// the query (dispatch_control_ready.go), which happens only when its parsed
// target is empty. workflowServeControlReadyQueryForBeads guarantees a
// non-empty GC_CONTROL_TARGET unconditionally, falling back to
// config.ControlDispatcherAgentName when agentCfg.QualifiedName() is blank
// (dispatch_runtime.go) -- so this test uses the zero-value Agent, the most
// adversarial input available, to confirm even that never yields an
// unrecognized query. If a future change ever lets target come back empty,
// this test fails first, flagging that routed_ready()/assignee_ready()'s
// hold-label handling has become load-bearing and needs a real fix, not
// just the string-level assertions above.
func TestWorkflowServeControlReadyQueryShellFallbackUnreachable(t *testing.T) {
	query := workflowServeControlReadyQuery(config.Agent{})
	parsed, ok := parseControlReadyQuery(query)
	if !ok || parsed.target == "" {
		t.Fatalf("parseControlReadyQuery(%q) = %+v, ok=%v; want ok=true with non-empty target -- shell fallback would become reachable", query, parsed, ok)
	}
}
