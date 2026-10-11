package main

import (
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/spf13/cobra"
)

// newRuntimeWorkdirExportCmd creates "gc runtime workdir-export", which
// lists the work-dir entries a runtime pack may copy into a remote box. It
// applies the same exclusions gc sends as work_dir_exclude, so packs call
// the gc binary instead of re-implementing the pattern grammar.
func newRuntimeWorkdirExportCmd(stdout, stderr io.Writer) *cobra.Command {
	var null bool
	cmd := &cobra.Command{
		Use:   "workdir-export <work_dir>",
		Short: "List the work-dir entries a runtime may copy off the orchestrator host",
		Long: `List the entries of a session work dir that a runtime may copy into another
filesystem (a remote sandbox, a container image).

Orchestrator-local state named by the work_dir_exclude start-config field
(secrets, the event log, the nudge queue, ledger stores, sockets, other
sessions' worktrees) is left out; an excluded directory is pruned with
everything below it. Paths are relative to <work_dir>, slash-separated, in
lexical order, one per line (NUL-terminated with --null).

Directories are listed as well as their contents so empty directories
survive, and symlinks are listed, never followed. Feed the list to a tool
without recursion, for example:

  gc runtime workdir-export --null "$WORK_DIR" |
    tar -C "$WORK_DIR" --null --no-recursion -T - -czf -

The protocol contract is docs/reference/exec-session-provider.md.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			entries, err := citylayout.WorkDirExportEntries(args[0])
			if err != nil {
				fmt.Fprintf(stderr, "gc runtime workdir-export: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			sep := "\n"
			if null {
				sep = "\x00"
			}
			for _, e := range entries {
				if _, err := io.WriteString(stdout, e+sep); err != nil {
					fmt.Fprintf(stderr, "gc runtime workdir-export: writing output: %v\n", err) //nolint:errcheck // best-effort stderr
					return errExit
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&null, "null", "0", false, "terminate entries with NUL instead of newline")
	return cmd
}
