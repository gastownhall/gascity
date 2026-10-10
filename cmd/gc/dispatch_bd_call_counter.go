package main

import (
	"sync/atomic"

	"github.com/gastownhall/gascity/internal/beads"
)

// controlBdCalls counts the bd commands this process's control stores run,
// split into reads and writes. The control-dispatcher serve loop traces the
// delta across each processed control (serve processed ... bd_reads=N
// bd_writes=M), which is the per-control cost every graph hop pays. Trace only:
// nothing reads it for a decision.
var controlBdCalls struct {
	reads  atomic.Int64
	writes atomic.Int64
}

// controlBdCallCount is a snapshot of controlBdCalls.
type controlBdCallCount struct {
	reads  int64
	writes int64
}

func snapshotControlBdCalls() controlBdCallCount {
	return controlBdCallCount{reads: controlBdCalls.reads.Load(), writes: controlBdCalls.writes.Load()}
}

func (c controlBdCallCount) since(start controlBdCallCount) controlBdCallCount {
	return controlBdCallCount{reads: c.reads - start.reads, writes: c.writes - start.writes}
}

// countControlBdCalls wraps a control store's command runner so every bd
// command it runs is counted in controlBdCalls.
func countControlBdCalls(runner beads.CommandRunner) beads.CommandRunner {
	return func(dir, name string, args ...string) ([]byte, error) {
		if name == "bd" {
			if bdArgsWrite(args) {
				controlBdCalls.writes.Add(1)
			} else {
				controlBdCalls.reads.Add(1)
			}
		}
		return runner(dir, name, args...)
	}
}

// bdArgsWrite reports whether a bd argv mutates the ledger, by its first verb
// (and the dep or label subcommand). Global flags and their values may
// precede the verb.
func bdArgsWrite(args []string) bool {
	for i, arg := range args {
		switch arg {
		case "create", "update", "close", "reopen", "delete", "batch":
			return true
		case "dep", "label":
			if i+1 < len(args) {
				switch args[i+1] {
				case "add", "remove", "rm":
					return true
				}
			}
			return false
		case "show", "list", "query", "ready", "sql", "version", "search", "count":
			return false
		}
	}
	return false
}
