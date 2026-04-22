package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

func TestCompleteSessionIDs_EarlyExitOnExtraArgs(t *testing.T) {
	// When the positional is already satisfied, the completer must return no
	// candidates and must not attempt to open the city store — otherwise it
	// would error out or emit noise for every keystroke after the ID is typed.
	got, dir := completeSessionIDs(nil, []string{"gc-42"}, "anything")
	if len(got) != 0 {
		t.Errorf("expected no candidates with args set, got %v", got)
	}
	if dir != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("expected NoFileComp directive, got %v", dir)
	}
}

func TestCompleteRigNames_EarlyExitOnExtraArgs(t *testing.T) {
	got, dir := completeRigNames(nil, []string{"myrig"}, "x")
	if len(got) != 0 {
		t.Errorf("expected no candidates, got %v", got)
	}
	if dir != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("expected NoFileComp directive, got %v", dir)
	}
}

func TestCompleteOrderNames_EarlyExitOnExtraArgs(t *testing.T) {
	got, dir := completeOrderNames(nil, []string{"some-order"}, "x")
	if len(got) != 0 {
		t.Errorf("expected no candidates, got %v", got)
	}
	if dir != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("expected NoFileComp directive, got %v", dir)
	}
}

func TestSessionCompletionDescription(t *testing.T) {
	cases := []struct {
		name string
		in   session.Info
		want string
	}{
		{"alias + state", session.Info{Alias: "mayor", State: session.State("asleep")}, "mayor (asleep)"},
		{"template fallback", session.Info{Template: "gascity/claude", State: session.State("active")}, "gascity/claude (active)"},
		{"empty state renders as closed", session.Info{Alias: "a"}, "a (closed)"},
		{"no alias and no template", session.Info{State: session.State("suspended")}, "- (suspended)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionCompletionDescription(tc.in)
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestOrderCompletionDescription(t *testing.T) {
	cases := []struct {
		name string
		in   orders.Order
		want string
	}{
		{"formula + interval", orders.Order{Formula: "f", Interval: "5m"}, "formula, 5m"},
		{"exec + schedule", orders.Order{Exec: "s", Schedule: "0 0 * * *"}, "exec, 0 0 * * *"},
		{"formula + event", orders.Order{Formula: "f", On: "bead.closed"}, "formula, bead.closed"},
		{"no timing", orders.Order{Formula: "f"}, "formula, -"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := orderCompletionDescription(tc.in)
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestQuietDefaultLogger_RestoresOutput(t *testing.T) {
	// The default logger's writer must be restored after fn returns, even if
	// fn panics or writes to it — otherwise a single noisy completion call
	// would leave the logger silenced for the rest of the process.
	var before bytes.Buffer
	log.SetOutput(&before)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	quietDefaultLogger(func() {
		log.Print("silenced")
	})
	if strings.Contains(before.String(), "silenced") {
		t.Errorf("expected log output to be suppressed inside quietDefaultLogger, got %q", before.String())
	}

	log.Print("audible")
	if !strings.Contains(before.String(), "audible") {
		t.Errorf("expected log output restored after quietDefaultLogger, got %q", before.String())
	}
}

func TestRigNameCandidates_LoadsAndFilters(t *testing.T) {
	// Integration check for the rig source-of-truth — exercises resolveCity
	// (via t.Chdir into a temp city), loadCityConfigFS, and the prefix filter.
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"my-city\"\n\n[[rigs]]\nname = \"alpha\"\npath = \"/tmp/alpha\"\n\n[[rigs]]\nname = \"beta\"\npath = \"/tmp/beta\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cityPath)

	got := rigNameCandidates("")
	// HQ + 2 rigs = 3 candidates.
	if len(got) != 3 {
		t.Fatalf("expected 3 candidates (HQ + 2 rigs), got %d: %v", len(got), got)
	}
	names := make([]string, len(got))
	for i, c := range got {
		names[i] = strings.SplitN(c, "\t", 2)[0]
	}
	for _, want := range []string{"my-city", "alpha", "beta"} {
		if !slicesContains(names, want) {
			t.Errorf("missing candidate %q in %v", want, names)
		}
	}

	// Prefix filter.
	got = rigNameCandidates("al")
	if len(got) != 1 || !strings.HasPrefix(got[0], "alpha\t") {
		t.Errorf("expected only alpha candidate for prefix 'al', got %v", got)
	}
}

func slicesContains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
