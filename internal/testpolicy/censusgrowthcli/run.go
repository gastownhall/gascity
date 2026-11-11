// Package censusgrowthcli implements the census growth report command.
package censusgrowthcli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/gastownhall/gascity/internal/testpolicy/censusgrowth"
)

const (
	exitError   = 1
	exitUsage   = 2
	exitBlocked = 3
)

// Run reads the four census files named by args, writes the markdown report to
// stdout and returns the process exit code. It reports and never decides: the
// exit is non-zero for growth or a policy-row change only under --enforce
// without --approved.
func Run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("census-growth", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var baseCounts, headCounts, basePolicy, headPolicy string
	var enforce, approved bool
	flags.StringVar(&baseCounts, "base-counts", "", "base test/test-resource-counts.toml (a missing or empty file means the base predates it)")
	flags.StringVar(&headCounts, "head-counts", "", "head test/test-resource-counts.toml")
	flags.StringVar(&basePolicy, "base-policy", "", "base test/test-resources.toml (a missing file is an empty policy)")
	flags.StringVar(&headPolicy, "head-policy", "", "head test/test-resources.toml")
	flags.BoolVar(&enforce, "enforce", false, "exit non-zero on GROWTH or POLICY-ROW unless --approved")
	flags.BoolVar(&approved, "approved", false, "a maintainer approved this change; only matters with --enforce")
	usage := func(err error) int {
		_, _ = fmt.Fprintf(stderr, "census-growth: %v\nusage: census-growth --base-counts F --head-counts F --base-policy F --head-policy F [--enforce] [--approved]\n", err)
		return exitUsage
	}
	if err := flags.Parse(args); err != nil {
		return usage(err)
	}
	if flags.NArg() > 0 {
		return usage(fmt.Errorf("unexpected argument %q", flags.Arg(0)))
	}
	for name, value := range map[string]string{"--base-counts": baseCounts, "--head-counts": headCounts, "--base-policy": basePolicy, "--head-policy": headPolicy} {
		if value == "" {
			return usage(fmt.Errorf("%s is required", name))
		}
	}

	in := censusgrowth.Input{}
	for _, read := range []struct {
		path     string
		dst      *[]byte
		label    string
		absentOK bool
	}{
		{baseCounts, &in.BaseCounts, "base counts", true},
		{basePolicy, &in.BasePolicy, "base policy", true},
		{headCounts, &in.HeadCounts, "head counts", false},
		{headPolicy, &in.HeadPolicy, "head policy", false},
	} {
		data, err := os.ReadFile(read.path)
		if errors.Is(err, fs.ErrNotExist) && read.absentOK {
			continue
		}
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "census-growth: read %s: %v\n", read.label, err)
			return exitError
		}
		*read.dst = data
	}

	report, err := censusgrowth.Compare(in)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "census-growth: %v\n", err)
		return exitError
	}
	if _, err := io.WriteString(stdout, report.Markdown()); err != nil {
		_, _ = fmt.Fprintf(stderr, "census-growth: write report: %v\n", err)
		return exitError
	}
	if enforce && !approved && (report.Growth() || report.PolicyRowChanged()) {
		_, _ = fmt.Fprintln(stderr, "census-growth: blocked: GROWTH or POLICY-ROW found; a maintainer approves it with --approved (the ledger-growth-approved label in CI)")
		return exitBlocked
	}
	return 0
}
