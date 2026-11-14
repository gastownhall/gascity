package main

import (
	"io"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/readywork"
)

// `gc ready --label` / `--label-any` (ga-878prh.1): the generated work queries of
// a route_label agent pass them to whichever reader the topology selects, and
// federated `gc ready` is the reader a split city selects. Without them a
// labeled agent on a split city would fail `unknown flag` on every poll.

func parseReadyFlagsForTest(t *testing.T, args ...string) readyFlags {
	t.Helper()
	var opts readyFlags
	var includeEphemeral, jsonOut bool
	cmd := &cobra.Command{Use: "ready"}
	registerReadyFlags(cmd, &opts, &includeEphemeral, &jsonOut)
	if err := cmd.Flags().Parse(args); err != nil {
		t.Fatalf("gc ready %v: %v", args, err)
	}
	return opts
}

func TestReadyRegistersLabelAndLabelAnyAsRepeatableFlags(t *testing.T) {
	opts := parseReadyFlagsForTest(t,
		"--label", "needs-claude-review", "--label=team:a",
		"--label-any", "urgent", "--label-any=vip,rush")
	if want := []string{"needs-claude-review", "team:a"}; !slices.Equal(opts.query.Labels, want) {
		t.Errorf("--label values = %v, want %v", opts.query.Labels, want)
	}
	// bd splits a comma-joined value itself; the reader receives it verbatim
	// and splits it at compile time, so the flag must not pre-split.
	if want := []string{"urgent", "vip,rush"}; !slices.Equal(opts.query.AnyLabels, want) {
		t.Errorf("--label-any values = %v, want %v", opts.query.AnyLabels, want)
	}
	if err := opts.query.Validate(); err != nil {
		t.Errorf("a labeled query does not validate: %v", err)
	}
}

// The same labeled argv over one store and over the federation of that store's
// rows must serve the same rows: federation changes where rows live, not which
// are served.
func TestReadyLabelsServeTheSameRowsSingleStoreAndFederated(t *testing.T) {
	seed := []beads.Bead{
		{Title: "both", Type: "task", Labels: []string{"review", "urgent"}},
		{Title: "review", Type: "task", Labels: []string{"review"}},
		{Title: "urgent", Type: "task", Labels: []string{"urgent", "vip"}},
		{Title: "held review urgent", Type: "task", Labels: []string{"review", "urgent", "hold:mayor"}},
		{Title: "plain", Type: "task"},
	}
	single := splittest.NewWorkStore(t, "gc")
	city := splittest.NewWorkStore(t, "gc")
	rig := splittest.NewWorkStore(t, "rg")
	for i, b := range seed {
		mustCreateReadyBead(t, single, b)
		// Federated: spread the same rows over a city leg and a rig leg.
		target := city
		if i%2 == 1 {
			target = rig
		}
		mustCreateReadyBead(t, target, b)
	}
	titleSet := func(rows []readywork.ReadyBead) []string {
		var out []string
		for _, r := range rows {
			out = append(out, r.Title)
		}
		slices.Sort(out)
		return out
	}

	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"label", []string{"--label", "review"}, []string{"both", "held review urgent", "review"}},
		{"label AND label", []string{"--label", "review", "--label", "urgent"}, []string{"both", "held review urgent"}},
		{"label-any", []string{"--label-any", "vip", "--label-any", "review"}, []string{"both", "held review urgent", "review", "urgent"}},
		{"label with label-any", []string{"--label", "review", "--label-any", "urgent"}, []string{"both", "held review urgent"}},
		{"with the hold exclusion", []string{"--label", "review", "--exclude-label", "hold:mayor"}, []string{"both", "review"}},
		{"nothing matches", []string{"--label", "absent"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := parseReadyFlagsForTest(t, tc.args...).query
			one, err := readywork.Read([]readywork.Leg{readyTestLeg("city", single)}, q)
			if err != nil {
				t.Fatalf("single store read: %v", err)
			}
			many, err := readywork.Read([]readywork.Leg{readyTestLeg("city", city), readyTestLeg("rig", rig)}, q)
			if err != nil {
				t.Fatalf("federated read: %v", err)
			}
			if got := titleSet(one); !slices.Equal(got, tc.want) {
				t.Errorf("single store served %v, want %v", got, tc.want)
			}
			if got := titleSet(many); !slices.Equal(got, tc.want) {
				t.Errorf("federated served %v, want %v", got, tc.want)
			}
		})
	}
}

// The steer a frontier refusal prints names exactly the flags `gc ready`
// accepts. The dynamic pin (TestGcReadySteerDescribesTheFlagsItActuallyAccepts)
// forces that once the flags register; this names the two flags so a regression
// of the steer text alone is caught with its own message.
func TestGcReadySteerAdvertisesTheLabelFlags(t *testing.T) {
	msg := beads.RelocatedClassFrontierRefusal("bd ready", []beads.RelocatedClass{
		{Class: "graph", IDPrefix: "gcg", Location: `the "infra" storage binding`},
	}).Error()
	const rejectsMarker = "rejects the rest of bd's ready surface ("
	advertised, rejected, split := strings.Cut(msg, rejectsMarker)
	if !split {
		t.Fatalf("the steer no longer separates accepted from rejected flags: %q", msg)
	}
	for _, flag := range []string{"--label", "--label-any"} {
		if !regexp.MustCompile(regexp.QuoteMeta(flag) + `([^-\w]|$)`).MatchString(advertised) {
			t.Errorf("the steer does not advertise %s as accepted: %q", flag, advertised)
		}
		if regexp.MustCompile(regexp.QuoteMeta(flag) + `([^-\w]|$)`).MatchString(rejected) {
			t.Errorf("the steer still lists %s as rejected: %q", flag, rejected)
		}
	}
	if err := newReadyCmd(io.Discard, io.Discard).ParseFlags([]string{"--label", "x", "--label-any", "y"}); err != nil {
		t.Errorf("gc ready rejects the label flags: %v", err)
	}
}

// Every filter the reader's query carries has a flag on the command: a filter
// reachable from the API and not from the CLI is the parity gap this closes.
func TestReadyEveryQueryFilterFieldHasACommandFlag(t *testing.T) {
	cmd := newReadyCmd(io.Discard, io.Discard)
	for _, flag := range []string{"label", "label-any", "exclude-label", "exclude-type", "metadata-field", "assignee", "unassigned"} {
		if cmd.Flags().Lookup(flag) == nil {
			t.Errorf("gc ready has no --%s flag", flag)
		}
	}
}
