package main

import (
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/packcap"
	"github.com/spf13/cobra"
)

func newPackDiffCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "diff <old-pack-dir> <new-pack-dir>",
		Short: "Classify the capability-surface change between two pack versions",
		Long: `Compare the capability surfaces of two versions of a pack and classify the change.

A pack's declared version does not carry compatibility, so this command
computes each pack directory's capability manifest (see "gc pack capability")
and compares the two. The six properties are PROVIDES, MANDATES, DEMANDS,
USES, NORMS and OPAQUE.

Verdicts and exit codes, highest precedence first:
  BREAKING      a commitment was removed or changed                     exit 2
  UNCLASSIFIED  a file moved that no computable change accounts for     exit 1
  ADDITIVE      a commitment was added and every moved file is new      exit 0
                with an added provider
  NONE          the manifests are identical                             exit 0

The verdict is the highest that applies: an addition never hides an
unexplained moved file. PROVIDES and USES: removal is breaking, addition is
additive. MANDATES and NORMS: any change is breaking. DEMANDS: addition is
breaking, removal is additive. OPAQUE digests every file in the pack; a moved
digest is UNCLASSIFIED, never NONE, unless the file was added or removed with
the agent, command, formula or order it belongs to. Read the listed files.

Errors (a missing directory, an unparseable TOML file) also exit 1; with
--json they print an ok:false failure payload instead of a verdict.

To compare a pack at two commits, extract the older tree first, for example:
  git archive <ref> path/to/pack | tar -x -C /tmp/old`,
		Example: `  gc pack diff /tmp/old/internal/bootstrap/packs/core internal/bootstrap/packs/core
  gc pack diff old-pack new-pack --json`,
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			return exitForCode(doPackDiff(args[0], args[1], jsonOutput, stdout, stderr))
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit the verdict and findings as JSON")
	return cmd
}

func newPackCapabilityCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "capability <pack-dir>",
		Short: "Print a pack directory's capability manifest",
		Long: `Print the capability manifest of a pack directory.

The manifest is sorted, one "PROPERTY<TAB>value" line per entry, and depends
only on the directory's contents:

  PROVIDES  commands, agents, formulas and orders the pack ships, including
            inline [[agent]] and [[commands]] in pack.toml
  MANDATES  claim and drain-ack commands its prose tells an agent to run
  DEMANDS   reserved gc.* metadata keys and versioned schema ids it requires
  USES      formula constructs it depends on (steps.check, steps.retry, ...)
            and each file's requires.formula_compiler with its value
  NORMS     must/never/always/do-not lines in role prompts and template
            fragments (.md, .template.md and .md.tmpl)
  OPAQUE    a 12-hex SHA-256 prefix per file, except .git

A symlinked pack directory is resolved first. Inside the pack, a symlink to a
regular file within the pack is read through; any other symlink is recorded
by its target string and never followed.

"gc pack diff" compares two manifests.`,
		Example: `  gc pack capability internal/bootstrap/packs/core
  gc pack capability ./my-pack --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return exitForCode(doPackCapability(args[0], jsonOutput, stdout, stderr))
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit the manifest as JSON")
	return cmd
}

type packCapabilityJSONResult struct {
	SchemaVersion string          `json:"schema_version"`
	Path          string          `json:"path"`
	Entries       []packcap.Entry `json:"entries"`
}

type packDiffJSONResult struct {
	SchemaVersion string            `json:"schema_version"`
	A             string            `json:"a"`
	B             string            `json:"b"`
	Verdict       packcap.Verdict   `json:"verdict"`
	ExitCode      int               `json:"exit_code"`
	Findings      []packcap.Finding `json:"findings"`
	MovedFiles    []string          `json:"moved_files"`
}

func doPackCapability(dir string, jsonOutput bool, stdout, stderr io.Writer) int {
	m, err := packcap.Compute(dir)
	if err != nil {
		fmt.Fprintf(stderr, "gc pack capability: %v\n", err) //nolint:errcheck
		return 1
	}
	if jsonOutput {
		entries := m.Entries
		if entries == nil {
			entries = []packcap.Entry{}
		}
		return writeCLIJSONLineOrExit(stdout, stderr, "gc pack capability", packCapabilityJSONResult{
			SchemaVersion: "1",
			Path:          dir,
			Entries:       entries,
		})
	}
	if err := m.WriteTSV(stdout); err != nil {
		fmt.Fprintf(stderr, "gc pack capability: %v\n", err) //nolint:errcheck
		return 1
	}
	return 0
}

func doPackDiff(a, b string, jsonOutput bool, stdout, stderr io.Writer) int {
	ma, err := packcap.Compute(a)
	if err != nil {
		fmt.Fprintf(stderr, "gc pack diff: %v\n", err) //nolint:errcheck
		return 1
	}
	mb, err := packcap.Compute(b)
	if err != nil {
		fmt.Fprintf(stderr, "gc pack diff: %v\n", err) //nolint:errcheck
		return 1
	}
	r := packcap.Diff(ma, mb)
	code := r.Verdict.ExitCode()

	if jsonOutput {
		findings := r.Findings
		if findings == nil {
			findings = []packcap.Finding{}
		}
		moved := r.MovedFiles
		if moved == nil {
			moved = []string{}
		}
		if writeCLIJSONLineOrExit(stdout, stderr, "gc pack diff", packDiffJSONResult{
			SchemaVersion: "1",
			A:             a,
			B:             b,
			Verdict:       r.Verdict,
			ExitCode:      code,
			Findings:      findings,
			MovedFiles:    moved,
		}) != 0 {
			return 1
		}
		return code
	}

	writePackDiffHuman(stdout, a, b, r)
	return code
}

func writePackDiffHuman(w io.Writer, a, b string, r packcap.Result) {
	fmt.Fprintf(w, "A  %s\nB  %s\n\n", a, b) //nolint:errcheck
	section := func(label, caption string, values []string) {
		fmt.Fprintf(w, "  %-12s %s (%d)\n", label, caption, len(values)) //nolint:errcheck
		for _, v := range values {
			fmt.Fprintf(w, "                 %s\n", v) //nolint:errcheck
		}
	}
	for _, f := range r.Findings {
		section(string(f.Verdict), f.Caption(), f.Values)
	}
	if len(r.MovedFiles) > 0 {
		section(string(packcap.Unclassified), "files whose digest moved", r.MovedFiles)
	}
	if len(r.Findings) > 0 || len(r.MovedFiles) > 0 {
		fmt.Fprintln(w) //nolint:errcheck
	}
	switch r.Verdict {
	case packcap.Unclassified:
		fmt.Fprintf(w, "  VERDICT  %s: files moved that nothing computable accounts for\n", r.Verdict) //nolint:errcheck
	case packcap.None:
		fmt.Fprintf(w, "  VERDICT  %s: the manifests are identical\n", r.Verdict) //nolint:errcheck
	default:
		fmt.Fprintf(w, "  VERDICT  %s\n", r.Verdict) //nolint:errcheck
	}
	if len(r.MovedFiles) > 0 && r.Verdict != packcap.Breaking {
		fmt.Fprint(w, `
  UNCLASSIFIED is not NONE. A prompt or script rewritten to behave
  differently with the same surface lands here, and nothing computable can
  tell which kind of rewrite it was. Read the listed files.
`) //nolint:errcheck
	}
}
