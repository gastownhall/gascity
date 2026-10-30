package shardbudget

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

const usage = `usage: shard-budget [--fail-on-imbalance] [--allow-missing] BEP_JSON [BEP_JSON ...]

Warns (or, with --fail-on-imbalance, fails) when a sharded test target's
slowest shard in any of the given --build_event_json_file files is far
above its median. See the shardbudget package doc for the thresholds.
`

var (
	errHelp    = flag.ErrHelp
	errMissing = errors.New("no BEP file at this path")
)

type runOptions struct {
	bepFiles        []string
	allowMissing    bool
	failOnImbalance bool
}

func parseArgs(args []string, stderr io.Writer) (runOptions, error) {
	opts := runOptions{}
	fs := flag.NewFlagSet("shard-budget", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = io.WriteString(stderr, usage) }
	fs.BoolVar(&opts.allowMissing, "allow-missing", false, "skip a BEP_JSON argument that does not exist instead of failing")
	fs.BoolVar(&opts.failOnImbalance, "fail-on-imbalance", false, "exit nonzero (and print ::error) past the fail threshold; default is warn-only")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() == 0 {
		return opts, errors.New("no BEP_JSON arguments")
	}
	opts.bepFiles = fs.Args()
	return opts, nil
}

// openBEP opens path, wrapping a not-exist error in errMissing so Run can
// tell "skip this lane, it never ran" apart from a real read failure.
func openBEP(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s: %w", path, errMissing)
		}
		return nil, err
	}
	return f, nil
}
