package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// route_label / route_label_any agents (ga-878prh.1) narrow what a template's
// workers are served, so the controller must narrow what it counts as demand by
// the same rules. These tests hold the Go predicate (demandServableForTemplateRules)
// and the generated query's flag form to ONE verdict per row, exactly as
// demand_serve_agreement_test.go does for the unlabeled agent.

const (
	labelReview = "needs-claude-review"
	labelUrgent = "urgent"
	labelVIP    = "vip"
)

// routeLabelAgreementAgents are the labeled shapes the agreement runs over:
// AND only, OR only, and both together (which combine with AND).
func routeLabelAgreementAgents() []config.Agent {
	return []config.Agent{
		{Name: "worker", Dir: "rig", RouteLabel: []string{labelReview}},
		{Name: "worker", Dir: "rig", RouteLabelAny: []string{labelUrgent, labelVIP}},
		{Name: "worker", Dir: "rig", RouteLabel: []string{labelReview}, RouteLabelAny: []string{labelUrgent, labelVIP}},
	}
}

// labelVariants are the label sets every corpus row is replayed with.
func labelVariants() [][]string {
	return [][]string{
		nil,
		{labelReview},
		{labelUrgent},
		{labelReview, labelUrgent},
		{labelVIP, labelReview},
		{"unrelated"},
	}
}

// routeLabelsSatisfied is the independent oracle: bd's semantics, written out.
func routeLabelsSatisfied(have, all, anyOf []string) bool {
	for _, want := range all {
		if !slices.Contains(have, want) {
			return false
		}
	}
	if len(anyOf) == 0 {
		return true
	}
	for _, want := range anyOf {
		if slices.Contains(have, want) {
			return true
		}
	}
	return false
}

func withLabels(b beads.Bead, labels []string) beads.Bead {
	b.Labels = append(slices.Clone(b.Labels), labels...)
	return b
}

func rulesFor(a config.Agent) config.PoolDemandServeRules {
	return config.PoolDemandServeRulesFor(&a)
}

func TestRouteLabelGoPredicateAndGeneratedQueryAgreeRowByRow(t *testing.T) {
	cfg := agreementConfig()
	for _, agent := range routeLabelAgreementAgents() {
		name := strings.Join(agent.RouteLabel, "+") + "|any:" + strings.Join(agent.RouteLabelAny, "+")
		t.Run(name, func(t *testing.T) {
			rules := rulesFor(agent)
			templates := map[string]config.PoolDemandServeRules{agreementTemplate: rules}
			query := agent.EffectiveWorkQueryFor(config.QueryTopology{})
			matches := parseReadyArgsForTest(t, tierThreeReaderArgs(t, query, agreementTemplate))
			legacyMatches := parseReadyArgsForTest(t, tierThreeLegacyReaderArgs(t, query, agreementTemplate))
			assertLegacyTierFilterUnchanged(t, query)

			for _, row := range append(agreementRows(), holdLabelAgreementRows()...) {
				for _, variant := range labelVariants() {
					bead := withLabels(postCanonicalizeBead(cfg, row.bead), variant)
					label := row.name + " labels=" + strings.Join(variant, ",")
					t.Run(label, func(t *testing.T) {
						_, counted := demandServableForTemplateRules(cfg, bead, templates)
						served := workerIsServed(bead, matches) || legacyWorkflowTierServes(bead, legacyMatches)
						if counted != served {
							t.Fatalf("AGREEMENT VIOLATED for %s labels %v: the Go demand predicate says %v, the generated query says %v\nquery: %s",
								bead.ID, bead.Labels, counted, served, query)
						}
						want := row.wantServable && routeLabelsSatisfied(bead.Labels, agent.RouteLabel, agent.RouteLabelAny)
						if counted != want {
							t.Fatalf("both representations say %v for %s labels %v, want %v", counted, bead.ID, bead.Labels, want)
						}
					})
				}
			}
		})
	}
}

// An agent with no route labels counts exactly what it did before: the rules
// path with the base rules is the unchanged predicate.
func TestRouteLabelEmptyRulesKeepTheUnlabeledPredicate(t *testing.T) {
	cfg := agreementConfig()
	base := map[string]config.PoolDemandServeRules{agreementTemplate: rulesFor(config.Agent{Name: "worker", Dir: "rig"})}
	legacy := map[string]struct{}{agreementTemplate: {}}
	for _, row := range append(agreementRows(), holdLabelAgreementRows()...) {
		for _, variant := range labelVariants() {
			bead := withLabels(postCanonicalizeBead(cfg, row.bead), variant)
			gotTemplate, got := demandServableForTemplateRules(cfg, bead, base)
			wantTemplate, want := demandServableForTemplates(cfg, bead, legacy)
			if got != want || gotTemplate != wantTemplate {
				t.Fatalf("%s labels %v: rules path (%q,%v) != unlabeled path (%q,%v)", row.name, variant, gotTemplate, got, wantTemplate, want)
			}
		}
	}
}

// One store group counts several templates; a row routed to a template is held
// to THAT template's labels, never another's.
func TestRouteLabelRulesAreAppliedPerTemplate(t *testing.T) {
	cfg := agreementConfig()
	labeled := rulesFor(config.Agent{Name: "worker", Dir: "rig", RouteLabel: []string{labelReview}})
	plain := rulesFor(config.Agent{Name: "solo", Dir: "rig"})
	templates := map[string]config.PoolDemandServeRules{"rig/worker": labeled, "rig/solo": plain}
	row := func(id, route string, labels ...string) beads.Bead {
		return beads.Bead{
			ID: id, Status: "open", Type: "task", Labels: labels,
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: route},
		}
	}
	tests := []struct {
		name string
		bead beads.Bead
		want string
	}{
		{"labeled template, matching row", row("p-1", "rig/worker", labelReview), "rig/worker"},
		{"labeled template, unlabeled row", row("p-2", "rig/worker"), ""},
		{"labeled template, other label only", row("p-3", "rig/worker", labelUrgent), ""},
		{"plain template ignores labels, labeled row", row("p-4", "rig/solo", labelReview), "rig/solo"},
		{"plain template, unlabeled row", row("p-5", "rig/solo"), "rig/solo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := demandServableForTemplateRules(cfg, tt.bead, templates)
			if want := tt.want != ""; ok != want || got != tt.want {
				t.Fatalf("demandServableForTemplateRules = (%q, %v), want (%q, %v)", got, ok, tt.want, want)
			}
		})
	}
}

// The slot-suffix collapse resolves the route to the pool template first; the
// labels are then held against the template's rules.
func TestRouteLabelAppliesToSlotSuffixedRoutes(t *testing.T) {
	cfg := agreementConfig()
	templates := map[string]config.PoolDemandServeRules{
		agreementTemplate: rulesFor(config.Agent{Name: "worker", Dir: "rig", RouteLabel: []string{labelReview}}),
	}
	suffixed := func(labels ...string) beads.Bead {
		return beads.Bead{
			ID: "s-1", Status: "open", Type: "task", Labels: labels,
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: agreementTemplate + "-2"},
		}
	}
	if got, ok := demandServableForTemplateRules(cfg, suffixed(labelReview), templates); !ok || got != agreementTemplate {
		t.Errorf("labeled slot-suffixed row = (%q, %v), want it counted for %s", got, ok, agreementTemplate)
	}
	if got, ok := demandServableForTemplateRules(cfg, suffixed(), templates); ok {
		t.Errorf("unlabeled slot-suffixed row counted for %q; the template requires %s", got, labelReview)
	}
}

// Matching labels never relax the base exclusions: a held, human, epic or
// already-assigned row is not demand however well it is labeled.
func TestRouteLabelNeverRelaxesTheBaseExclusions(t *testing.T) {
	cfg := agreementConfig()
	templates := map[string]config.PoolDemandServeRules{
		agreementTemplate: rulesFor(config.Agent{Name: "worker", Dir: "rig", RouteLabel: []string{labelReview}}),
	}
	base := func() beads.Bead {
		return beads.Bead{
			ID: "x-1", Status: "open", Type: "task", Labels: []string{labelReview},
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: agreementTemplate},
		}
	}
	if _, ok := demandServableForTemplateRules(cfg, base(), templates); !ok {
		t.Fatal("control row: a routed, labeled, unassigned task must be counted")
	}
	for _, hold := range append(slices.Clone(beadmeta.DispatchHoldLabels), beadmeta.HumanLabel) {
		b := base()
		b.Labels = append(b.Labels, hold)
		if got, ok := demandServableForTemplateRules(cfg, b, templates); ok {
			t.Errorf("row carrying %q counted for %q despite matching route labels", hold, got)
		}
	}
	epic := base()
	epic.Type = "epic"
	if _, ok := demandServableForTemplateRules(cfg, epic, templates); ok {
		t.Error("a labeled epic was counted")
	}
	owned := base()
	owned.Assignee = "someone"
	if _, ok := demandServableForTemplateRules(cfg, owned, templates); ok {
		t.Error("a labeled, already-assigned row was counted as unclaimed demand")
	}
}

// A queue holding only label-mismatched rows is zero demand: no seat is minted
// for work the template's workers are never served.
func TestRouteLabelMismatchedQueueIsZeroDemand(t *testing.T) {
	cfg := agreementConfig()
	templates := map[string]config.PoolDemandServeRules{
		agreementTemplate: rulesFor(config.Agent{Name: "worker", Dir: "rig", RouteLabelAny: []string{labelUrgent}}),
	}
	for i, labels := range [][]string{nil, {labelReview}, {"other"}} {
		b := beads.Bead{
			ID: "m-" + string(rune('a'+i)), Status: "open", Type: "task", Labels: labels,
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: agreementTemplate},
		}
		if got, ok := demandServableForTemplateRules(cfg, b, templates); ok {
			t.Errorf("row with labels %v counted for %q; the template requires one of %s", labels, got, labelUrgent)
		}
	}
}

// Control rows keep the dispatcher-ownership rule when rules carry labels: the
// labels are an additional requirement, never a substitute for ownership.
func TestRouteLabelKeepsControlRowOwnership(t *testing.T) {
	const (
		cityDispatcher = "core.control-dispatcher"
		rigDispatcher  = "fixture/core.control-dispatcher"
	)
	cfg := classBindingDispatcherFixtureConfig(t)
	required := config.PoolDemandServeRules{
		RequireUnassigned: true, RequireLabels: []string{labelReview},
		ExcludeTypes: []string{"epic"}, ExcludeLabels: append(slices.Clone(beadmeta.DispatchHoldLabels), beadmeta.HumanLabel),
	}
	templates := map[string]config.PoolDemandServeRules{cityDispatcher: required, rigDispatcher: required}
	row := func(route, rootRef string, labels ...string) beads.Bead {
		return beads.Bead{ID: "c-1", Status: "open", Type: "task", Labels: labels, Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:     route,
			beadmeta.KindMetadataKey:         beadmeta.KindWorkflowFinalize,
			beadmeta.RootStoreRefMetadataKey: rootRef,
		}}
	}
	if got, ok := demandServableForTemplateRules(cfg, row(cityDispatcher, "city:test-city", labelReview), templates); !ok || got != cityDispatcher {
		t.Errorf("owned, labeled control row = (%q, %v), want it counted for %s", got, ok, cityDispatcher)
	}
	if got, ok := demandServableForTemplateRules(cfg, row(cityDispatcher, "city:test-city"), templates); ok {
		t.Errorf("owned but unlabeled control row counted for %q", got)
	}
	if got, ok := demandServableForTemplateRules(cfg, row(cityDispatcher, "rig:fixture", labelReview), templates); ok {
		t.Errorf("cross-scope control row counted for %q because its labels matched", got)
	}
}
