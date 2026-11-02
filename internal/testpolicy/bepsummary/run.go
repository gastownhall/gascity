package bepsummary

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const usage = `usage: bazel-bep-summary [--context NAME] [--json-out PATH] [--allow-missing] [--top N] PHASE=BEP_JSON [PHASE=BEP_JSON ...]

Summarizes Bazel --build_event_json_file output: test targets cached
(local / remote / disk) vs executed (remote / local), outcomes, and action
runner/cache statistics. Markdown goes to stdout; --json-out writes the
machine-readable report.
`

const defaultTop = 10

type runOptions struct {
	context      string
	jsonOut      string
	allowMissing bool
	top          int
	phases       []phaseArg
}

type phaseArg struct {
	label string
	path  string
}

// Run is the bazel-bep-summary command line. It returns the process exit
// code: 0 on success, 1 on read/parse/write failure, 2 on usage errors.
func Run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args, stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprintf(stderr, "bazel-bep-summary: %v\n%s", err, usage)
		}
		return 2
	}
	phases := make([]Phase, 0, len(opts.phases))
	for _, pa := range opts.phases {
		p, err := summarizeFile(pa, opts.allowMissing)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "bazel-bep-summary: %v\n", err)
			return 1
		}
		if p.Missing {
			_, _ = fmt.Fprintf(stderr, "bazel-bep-summary: phase %q: no BEP file at %s\n", pa.label, pa.path)
		}
		if p.Truncated {
			_, _ = fmt.Fprintf(stderr, "bazel-bep-summary: phase %q: %s ends in a truncated event (interrupted build?)\n", pa.label, pa.path)
		}
		phases = append(phases, p)
	}
	rep := BuildReport(opts.context, phases, opts.top)
	if err := WriteMarkdown(stdout, rep); err != nil {
		_, _ = fmt.Fprintf(stderr, "bazel-bep-summary: writing markdown: %v\n", err)
		return 1
	}
	if opts.jsonOut != "" {
		if err := writeJSONAtomic(opts.jsonOut, rep); err != nil {
			_, _ = fmt.Fprintf(stderr, "bazel-bep-summary: %v\n", err)
			return 1
		}
	}
	return 0
}

func parseArgs(args []string, stderr io.Writer) (runOptions, error) {
	opts := runOptions{}
	fs := flag.NewFlagSet("bazel-bep-summary", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = io.WriteString(stderr, usage) }
	fs.StringVar(&opts.context, "context", "", "where the invocations ran (pre-push, pr, main, local, ...)")
	fs.StringVar(&opts.jsonOut, "json-out", "", "write the JSON report to this path")
	fs.BoolVar(&opts.allowMissing, "allow-missing", false, "report a missing BEP file as a 'no BEP file' phase instead of failing")
	fs.IntVar(&opts.top, "top", defaultTop, "number of slowest executed tests to list")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if opts.top < 0 {
		return opts, fmt.Errorf("--top must be >= 0, got %d", opts.top)
	}
	if fs.NArg() == 0 {
		return opts, errors.New("no PHASE=BEP_JSON arguments")
	}
	seen := map[string]bool{}
	for _, a := range fs.Args() {
		label, path, ok := strings.Cut(a, "=")
		if !ok || label == "" || path == "" {
			return opts, fmt.Errorf("argument %q is not PHASE=BEP_JSON", a)
		}
		if seen[label] {
			return opts, fmt.Errorf("phase %q given twice", label)
		}
		seen[label] = true
		opts.phases = append(opts.phases, phaseArg{label: label, path: path})
	}
	return opts, nil
}

func summarizeFile(pa phaseArg, allowMissing bool) (Phase, error) {
	f, err := os.Open(pa.path)
	if err != nil {
		if allowMissing && errors.Is(err, os.ErrNotExist) {
			return Phase{Label: pa.label, Source: pa.path, Missing: true}, nil
		}
		return Phase{}, fmt.Errorf("phase %q: %w", pa.label, err)
	}
	defer func() { _ = f.Close() }()
	return SummarizePhase(pa.label, pa.path, f)
}

func writeJSONAtomic(path string, rep Report) error {
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding JSON report: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".bep-summary-*.json")
	if err != nil {
		return fmt.Errorf("writing JSON report %s: %w", path, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("writing JSON report %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("writing JSON report %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("writing JSON report %s: %w", path, err)
	}
	return nil
}
