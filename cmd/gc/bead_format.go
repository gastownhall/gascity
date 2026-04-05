package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/gastownhall/gascity/internal/beads"
)

// parseBeadFormat extracts --format/--json flags from raw args (needed because
// DisableFlagParsing is true). Returns the format ("text", "json", or "toon")
// and the remaining positional args with the flag removed.
func parseBeadFormat(args []string) (string, []string) {
	format := "text"
	var rest []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--format" && i+1 < len(args):
			format = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--format="):
			format = strings.TrimPrefix(args[i], "--format=")
		case args[i] == "--json":
			format = "json"
		default:
			rest = append(rest, args[i])
		}
	}
	return format, rest
}

// beadFilters holds optional --label and --status flags parsed from args.
type beadFilters struct {
	label  string
	status string
	all    bool
}

// parseBeadFilters extracts --label=X and --status=X from args, returning
// the filters and the remaining args with those flags removed.
func parseBeadFilters(args []string) (beadFilters, []string) {
	var f beadFilters
	var rest []string
	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--label="):
			f.label = strings.TrimPrefix(args[i], "--label=")
		case args[i] == "--label" && i+1 < len(args):
			f.label = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--status="):
			f.status = strings.TrimPrefix(args[i], "--status=")
		case args[i] == "--status" && i+1 < len(args):
			f.status = args[i+1]
			i++
		case args[i] == "--all":
			f.all = true
		default:
			rest = append(rest, args[i])
		}
	}
	return f, rest
}

// filterBeads returns beads matching the given filters. Empty filter fields
// match everything.
func filterBeads(bs []beads.Bead, f beadFilters) []beads.Bead {
	if f.label == "" && f.status == "" {
		return bs
	}
	var out []beads.Bead
	for _, b := range bs {
		if f.status != "" && b.Status != f.status {
			continue
		}
		if f.label != "" {
			found := false
			for _, l := range b.Labels {
				if l == f.label {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		out = append(out, b)
	}
	return out
}

// writeBeadJSON writes a single bead as indented JSON.
func writeBeadJSON(b beads.Bead, stdout io.Writer) {
	data, _ := json.MarshalIndent(b, "", "  ")
	fmt.Fprintln(stdout, string(data)) //nolint:errcheck // best-effort stdout
}

// writeBeadsJSON writes a slice of beads as a JSON array.
func writeBeadsJSON(bs []beads.Bead, stdout io.Writer) {
	data, _ := json.MarshalIndent(bs, "", "  ")
	fmt.Fprintln(stdout, string(data)) //nolint:errcheck // best-effort stdout
}

// writeBeadDetail writes a single bead in human-readable detail format.
func writeBeadDetail(b beads.Bead, stdout io.Writer) {
	w := func(s string) { fmt.Fprintln(stdout, s) } //nolint:errcheck // best-effort stdout
	w(fmt.Sprintf("ID:       %s", b.ID))
	w(fmt.Sprintf("Status:   %s", b.Status))
	w(fmt.Sprintf("Type:     %s", b.Type))
	w(fmt.Sprintf("Title:    %s", b.Title))
	w(fmt.Sprintf("Created:  %s", b.CreatedAt.Format("2006-01-02 15:04:05")))
	assignee := b.Assignee
	if assignee == "" {
		assignee = "\u2014"
	}
	w(fmt.Sprintf("Assignee: %s", assignee))
}

// writeBeadTable writes beads in a tab-aligned table. If showAssignee is true,
// includes the ASSIGNEE column.
func writeBeadTable(bs []beads.Bead, stdout io.Writer, showAssignee bool) {
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	if showAssignee {
		fmt.Fprintln(tw, "ID\tSTATUS\tASSIGNEE\tTITLE") //nolint:errcheck // best-effort stdout
		for _, b := range bs {
			assignee := b.Assignee
			if assignee == "" {
				assignee = "\u2014"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", b.ID, b.Status, assignee, b.Title) //nolint:errcheck // best-effort stdout
		}
	} else {
		fmt.Fprintln(tw, "ID\tSTATUS\tTITLE") //nolint:errcheck // best-effort stdout
		for _, b := range bs {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", b.ID, b.Status, b.Title) //nolint:errcheck // best-effort stdout
		}
	}
	tw.Flush() //nolint:errcheck // best-effort stdout
}

// toonVal quotes a TOON value if it contains commas, quotes, or newlines.
func toonVal(s string) string {
	if strings.ContainsAny(s, ",\"\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}
