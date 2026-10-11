package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"

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
everything below it. Paths are relative to <work_dir> and slash-separated,
in depth-first walk order with each directory's entries sorted by name.

Every surviving file and symlink is listed, plus every surviving directory
that is empty on disk. A non-empty directory is never listed, so a consumer
that recurses into listed entries still copies nothing excluded. Symlinks
are listed, never followed; copy them as links.

Scripts must use --null, which terminates entries with NUL. The default
newline-terminated output is for reading: it fails, printing nothing, when an
entry contains a newline or carriage return or starts with "-", because a
line-based consumer would misread it. Run the gc that produced the start
config (its env.GC_BIN) and check its exit status, for example:

  set -o pipefail
  "$GC_BIN" runtime workdir-export --null "$WORK_DIR" |
    tar -C "$WORK_DIR" --null --no-recursion -T - -czf -

The protocol contract is docs/reference/exec-session-provider.md.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := writeWorkDirExport(stdout, args[0], null); err != nil {
				fmt.Fprintf(stderr, "gc runtime workdir-export: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&null, "null", "0", false, "terminate entries with NUL instead of newline (required for scripts)")
	return cmd
}

// writeWorkDirExport writes the surviving entries of workDir to w,
// NUL-terminated when null is set. Newline output is refused, before
// anything is written, when an entry would be ambiguous to a line-based
// consumer.
func writeWorkDirExport(w io.Writer, workDir string, null bool) error {
	entries, err := citylayout.WorkDirExportEntries(workDir)
	if err != nil {
		return err
	}
	sep := "\x00"
	if !null {
		sep = "\n"
		for _, e := range entries {
			if strings.ContainsAny(e, "\n\r") || strings.HasPrefix(e, "-") {
				return fmt.Errorf("entry %q cannot be listed one per line; use --null", e)
			}
		}
	}
	bw := bufio.NewWriter(w)
	for _, e := range entries {
		if _, err := bw.WriteString(e + sep); err != nil {
			return fmt.Errorf("writing output: %w", err)
		}
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	return nil
}
