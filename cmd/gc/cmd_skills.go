package main

import (
	"embed"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

//go:embed skills/*.md
var skillContents embed.FS

// skillTopics defines the available skill topics with their descriptions.
// Used by the gc skills command (content lookup) and stub materialization.
var skillTopics = []struct {
	Name string // skill name, e.g. "gc-work"
	Desc string // frontmatter description
	Arg  string // gc skills argument, e.g. "work"
}{
	{"gc-work", "Finding, creating, claiming, and closing work items (beads)", "work"},
	{"gc-dispatch", "Routing work to agents with gc sling and formulas", "dispatch"},
	{"gc-agents", "Managing agents — list, peek, nudge, suspend, drain", "agents"},
	{"gc-rigs", "Managing rigs — add, list, status, suspend, resume", "rigs"},
	{"gc-mail", "Sending and reading messages between agents", "mail"},
	{"gc-city", "City lifecycle — status, start, stop, init", "city"},
	{"gc-dashboard", "API server and web dashboard — config, start, monitor", "dashboard"},
}

func newSkillsCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills [topic]",
		Short: "Show command reference for a topic",
		Long: `Show curated command reference for a Gas City topic.

Without arguments, lists available topics. With a topic name,
prints the full command reference for that topic.`,
		Example: `  gc skills work       # beads command reference
  gc skills dispatch   # sling and formula reference
  gc skills            # list all topics`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				listSkillTopics(stdout)
				return nil
			}
			topic := args[0]
			content, err := fs.ReadFile(skillContents, "skills/"+topic+".md")
			if err != nil {
				fmt.Fprintf(stderr, "gc skills: unknown topic %q\n", topic) //nolint:errcheck // best-effort stderr
				fmt.Fprintln(stderr, "Available topics:")                   //nolint:errcheck // best-effort stderr
				listSkillTopics(stderr)
				return errExit
			}
			fmt.Fprint(stdout, string(content)) //nolint:errcheck // best-effort stdout
			return nil
		},
	}
	return cmd
}

// listSkillTopics prints available skill topics with descriptions.
func listSkillTopics(w io.Writer) {
	sorted := make([]struct{ Arg, Desc string }, len(skillTopics))
	for i, t := range skillTopics {
		sorted[i] = struct{ Arg, Desc string }{t.Arg, t.Desc}
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Arg < sorted[j].Arg
	})
	maxLen := 0
	for _, t := range sorted {
		if len(t.Arg) > maxLen {
			maxLen = len(t.Arg)
		}
	}
	for _, t := range sorted {
		pad := strings.Repeat(" ", maxLen-len(t.Arg))
		fmt.Fprintf(w, "  %s%s  %s\n", t.Arg, pad, t.Desc) //nolint:errcheck // best-effort
	}
}
