package main

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The open-control demand floor must not wake a dispatcher for rows its
// unassigned-work serving rules exclude, including on the legacy bare route.
func TestControlDispatcherDemandExcludesUnservableLabels(t *testing.T) {
	cfg := labelDemandDispatcherConfig()
	template := cfg.Agents[0].QualifiedName()
	for _, route := range []string{template, config.ControlDispatcherAgentName} {
		t.Run(route, func(t *testing.T) {
			for _, tc := range []struct {
				name   string
				labels [][]string
				want   int
			}{
				{name: "empty"},
				{name: "human", labels: [][]string{{beadmeta.HumanLabel}}},
				{name: "hold-mayor", labels: [][]string{{beadmeta.HoldMayorLabel}}},
				{name: "hold-external", labels: [][]string{{beadmeta.HoldExternalLabel}}},
				{name: "hold-mayor-case-and-space", labels: [][]string{{" Hold:Mayor "}}},
				{name: "hold-external-case-and-space", labels: [][]string{{"\tHOLD:EXTERNAL\n"}}},
				{name: "clean", labels: [][]string{nil}, want: 1},
				{name: "mixed-case-human", labels: [][]string{{"Human"}}, want: 1},
				{name: "upper-case-human", labels: [][]string{{"HUMAN"}}, want: 1},
				{name: "human-like", labels: [][]string{{"human-review"}}, want: 1},
				{name: "unrelated-hold", labels: [][]string{{"hold:review"}}, want: 1},
				{name: "excluded-after-unrelated-label", labels: [][]string{{"ready", beadmeta.HumanLabel}}},
				{name: "all-excluded", labels: [][]string{{beadmeta.HumanLabel}, {beadmeta.HoldExternalLabel}}},
				{name: "excluded-then-clean", labels: [][]string{{beadmeta.HumanLabel}, {beadmeta.HoldMayorLabel}, nil}, want: 1},
				{name: "clean-then-excluded", labels: [][]string{nil, {beadmeta.HumanLabel}, {beadmeta.HoldExternalLabel}}, want: 1},
				{name: "multiple-eligible-rows", labels: [][]string{nil, {"Human"}}, want: 1},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var rows []beads.Bead
					for i, labels := range tc.labels {
						rows = append(rows, labelDemandControlRow(fmt.Sprintf("control-%d", i), route, labels))
					}
					got := mergeCollectedDemand(cfg, collectedDemand{UnassignedRouted: rows})
					if count := got.ScaleCheckCounts[template]; count != tc.want {
						t.Errorf("control demand floor = %d, want %d for labels %q; counts=%v", count, tc.want, tc.labels, got.ScaleCheckCounts)
					}
				})
			}
		})
	}
}

func TestControlDispatcherLabelDemandPreservesNonlabelSelection(t *testing.T) {
	cfg := labelDemandDispatcherConfig()
	template := cfg.Agents[0].QualifiedName()
	for _, tc := range []struct {
		name   string
		status string
		owner  string
		route  string
	}{
		{name: "assigned", status: "open", owner: "existing-session", route: template},
		{name: "in-progress", status: "in_progress", route: template},
		{name: "closed", status: "closed", route: template},
		{name: "another-route", status: "open", route: "worker"},
		{name: "unrouted", status: "open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := labelDemandControlRow("control-work", tc.route, nil)
			row.Status, row.Assignee = tc.status, tc.owner
			got := mergeCollectedDemand(cfg, collectedDemand{UnassignedRouted: []beads.Bead{row}})
			if len(got.ScaleCheckCounts) != 0 {
				t.Fatalf("ineligible row supplied control demand: %v", got.ScaleCheckCounts)
			}
		})
	}
}

// This composition proof checks the observable cold-pool plan as well as the
// count, so a corrected predicate that is bypassed by planning still fails.
func TestColdControlDispatcherPlansOnlyForEligibleWork(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels [][]string
		want   int
	}{
		{name: "excluded-only", labels: [][]string{{beadmeta.HumanLabel}, {beadmeta.HoldExternalLabel}}},
		{name: "clean", labels: [][]string{nil}, want: 1},
		{name: "mixed", labels: [][]string{{beadmeta.HumanLabel}, nil}, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := labelDemandDispatcherConfig()
			template := cfg.Agents[0].QualifiedName()
			f := newAllocFixture(t, cfg)
			for i, labels := range tc.labels {
				f.in.Demand.Collected.UnassignedRouted = append(f.in.Demand.Collected.UnassignedRouted, labelDemandControlRow(fmt.Sprintf("control-%d", i), template, labels))
				f.in.Demand.Collected.UnassignedRoutedRefs = append(f.in.Demand.Collected.UnassignedRoutedRefs, "city:city")
			}
			got := f.decide()
			if count := got.Snapshot.PoolDesired[template]; count != tc.want {
				t.Errorf("cold dispatcher desired seats = %d, want %d", count, tc.want)
			}
			if len(got.Plans) != tc.want {
				t.Fatalf("cold dispatcher plans = %+v, want %d plans", got.Plans, tc.want)
			}
			for _, plan := range got.Plans {
				if plan.Template != template || plan.Kind != createPool {
					t.Errorf("cold plan = %+v, want pool plan for %s", plan, template)
				}
			}
		})
	}
}

func labelDemandDispatcherConfig() *config.City {
	return &config.City{Agents: []config.Agent{{
		Name:              config.ControlDispatcherAgentName,
		BindingName:       "core",
		StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(1),
	}}}
}

func labelDemandControlRow(id, route string, labels []string) beads.Bead {
	return beads.Bead{
		ID: id, Type: "task", Status: "open", Labels: labels,
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:     beadmeta.KindRetry,
			beadmeta.RoutedToMetadataKey: route,
		},
	}
}
