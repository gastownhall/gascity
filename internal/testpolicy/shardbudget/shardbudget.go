// Package shardbudget warns (or fails) when a sharded Bazel test target's
// slowest shard is far above its median, reading the same
// --build_event_json_file lanes already write for bepsummary.
//
// Rationale (rbe-ci-cost-latency-study.md, recommendation 4): an unbalanced
// shard split quietly grows the critical path of whichever lane runs it.
// gascity #7233/#7241 showed the gain of re-splitting a hot shard (one
// acceptance shard at 405s against a mean of 83s); this package keeps that
// gain from regressing. It also catches beads' server-Dolt storage tier,
// whose last shard trailed the rest by 4.3-4.5 min in 2 of 8 runs (study
// Section 3.4) -- a shape this package's thresholds are tuned to catch.
//
// For every sharded test target (more than one shard reported an executed
// result this invocation) it computes:
//
//	median  = the median wall time of the shards that executed (a cache hit
//	          says nothing about this run's balance, so it is excluded)
//	slowest = the maximum of those
//
// Thresholds (study Section 6.4):
//
//	warn when slowest > max(150s, 2 x median)
//	fail when slowest > max(300s, 3 x median)
//
// Day one: Run always prints "::warning" and exits 0; CI must not block on
// this yet. --fail-on-imbalance switches that to "::error" and a nonzero
// exit, once the budget has run clean for a while.
package shardbudget

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
)

// Warn and fail thresholds (study Section 6.4). A floor keeps a fast,
// lopsided split (two 5s shards and one 20s shard) from warning on ratio
// alone; a factor catches a slow target whose absolute lag is still small
// relative to its own median.
const (
	WarnFloorSeconds = 150.0
	WarnFactor       = 2.0
	FailFloorSeconds = 300.0
	FailFactor       = 3.0
)

// int64Value decodes a proto3-JSON integer. Bazel writes int64 fields as
// strings ("38") and int32 fields as numbers; both decode here (same
// handling as internal/testpolicy/bepsummary's bep.go).
type int64Value int64

func (v *int64Value) UnmarshalJSON(data []byte) error {
	data = bytes.Trim(data, `"`)
	if len(data) == 0 || string(data) == "null" {
		*v = 0
		return nil
	}
	n, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil {
		return fmt.Errorf("integer field %q: %w", data, err)
	}
	*v = int64Value(n)
	return nil
}

// bepEvent is the subset of a BEP testResult event this package reads.
type bepEvent struct {
	ID struct {
		TestResult *bepTestID `json:"testResult"`
	} `json:"id"`
	TestResult *bepTestResult `json:"testResult"`
}

type bepTestID struct {
	Label string     `json:"label"`
	Shard int64Value `json:"shard"`
}

type bepTestResult struct {
	CachedLocally             bool       `json:"cachedLocally"`
	TestAttemptDurationMillis int64Value `json:"testAttemptDurationMillis"`
	ExecutionInfo             struct {
		CachedRemotely bool `json:"cachedRemotely"`
	} `json:"executionInfo"`
}

// ShardDurations maps a test target's label to the executed duration (in
// milliseconds) of each of its shards this invocation. Shard indices start
// at 1, matching Bazel's BEP (which omits the field entirely when
// shard_count == 1).
type ShardDurations map[string]map[int64]int64

// ReadShardDurations decodes a newline-delimited BEP JSON stream and
// returns the executed (non-cached) duration of every shard any testResult
// event reported. A shard that reported more than one executed attempt
// (a retry) keeps the longest; this mirrors beads' tools/bazel/shard_budget.py
// so the two budgets agree on a shape. A malformed line is skipped, not an
// error: the same tolerance bepsummary's readStream gives a truncated or
// corrupted BEP file.
func ReadShardDurations(r io.Reader) ShardDurations {
	out := ShardDurations{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev bepEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		id := ev.ID.TestResult
		tr := ev.TestResult
		if id == nil || tr == nil {
			continue
		}
		if tr.CachedLocally || tr.ExecutionInfo.CachedRemotely {
			continue
		}
		shard := int64(id.Shard)
		if shard == 0 {
			shard = 1
		}
		perLabel := out[id.Label]
		if perLabel == nil {
			perLabel = map[int64]int64{}
			out[id.Label] = perLabel
		}
		ms := int64(tr.TestAttemptDurationMillis)
		if ms > perLabel[shard] {
			perLabel[shard] = ms
		}
	}
	return out
}

// Imbalance is one sharded test target's balance verdict for this
// invocation.
type Imbalance struct {
	Label         string
	Shards        int
	MedianMillis  int64
	SlowestMillis int64
	SlowestShard  int64
}

// WarnThresholdMillis is the slowest-shard duration above which Warn is true.
func (im Imbalance) WarnThresholdMillis() int64 {
	return maxInt64(int64(WarnFloorSeconds*1000), int64(WarnFactor*float64(im.MedianMillis)))
}

// FailThresholdMillis is the slowest-shard duration above which Fail is true.
func (im Imbalance) FailThresholdMillis() int64 {
	return maxInt64(int64(FailFloorSeconds*1000), int64(FailFactor*float64(im.MedianMillis)))
}

// Warn reports whether this target's slowest shard crossed the warn threshold.
func (im Imbalance) Warn() bool {
	return im.SlowestMillis > im.WarnThresholdMillis()
}

// Fail reports whether this target's slowest shard crossed the fail threshold.
func (im Imbalance) Fail() bool {
	return im.SlowestMillis > im.FailThresholdMillis()
}

// Message is the one-line, human-readable verdict Run prints per target.
func (im Imbalance) Message() string {
	return fmt.Sprintf(
		"%s: shard %d of %d took %.0fs against a median of %.0fs (warn over %.0fs, fail over %.0fs)",
		im.Label, im.SlowestShard, im.Shards,
		float64(im.SlowestMillis)/1000, float64(im.MedianMillis)/1000,
		float64(im.WarnThresholdMillis())/1000, float64(im.FailThresholdMillis())/1000,
	)
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// median returns the median of a slice of millisecond durations. For an
// even count it is the mean of the two middle elements (matching Python's
// statistics.median, which beads' tools/bazel/shard_budget.py uses), not the
// lower of the two: TestFloorAppliesBelowTinyMedians in the test file below
// depends on an odd count to land on an exact element instead.
func median(sorted []int64) int64 {
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// Evaluate returns one Imbalance for every target with at least two
// executed shards this invocation (fewer is not enough evidence of
// balance), sorted by slowest shard duration, descending.
func Evaluate(durations ShardDurations) []Imbalance {
	var out []Imbalance
	for label, perShard := range durations {
		if len(perShard) < 2 {
			continue
		}
		shardIndices := make([]int64, 0, len(perShard))
		for s := range perShard {
			shardIndices = append(shardIndices, s)
		}
		values := make([]int64, 0, len(perShard))
		for _, s := range shardIndices {
			values = append(values, perShard[s])
		}
		sortedValues := append([]int64(nil), values...)
		sort.Slice(sortedValues, func(i, j int) bool { return sortedValues[i] < sortedValues[j] })
		med := median(sortedValues)

		var slowestShard int64
		var slowestMillis int64 = -1
		for _, s := range shardIndices {
			if perShard[s] > slowestMillis {
				slowestMillis = perShard[s]
				slowestShard = s
			}
		}
		out = append(out, Imbalance{
			Label:         label,
			Shards:        len(perShard),
			MedianMillis:  med,
			SlowestMillis: slowestMillis,
			SlowestShard:  slowestShard,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SlowestMillis != out[j].SlowestMillis {
			return out[i].SlowestMillis > out[j].SlowestMillis
		}
		return out[i].Label < out[j].Label // stable tie-break
	})
	return out
}

// Run is the shard-budget command line. It returns the process exit code:
// 0 unless --fail-on-imbalance is given and some target's slowest shard
// crossed the fail threshold (1), an input could not be read (1), or the
// arguments themselves are invalid (2).
func Run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args, stderr)
	if err != nil {
		if !errors.Is(err, errHelp) {
			_, _ = fmt.Fprintf(stderr, "shard-budget: %v\n%s", err, usage)
		}
		return 2
	}

	merged := ShardDurations{}
	for _, path := range opts.bepFiles {
		f, err := openBEP(path)
		if err != nil {
			if opts.allowMissing && errors.Is(err, errMissing) {
				continue
			}
			_, _ = fmt.Fprintf(stderr, "shard-budget: %v\n", err)
			return 1
		}
		durations := ReadShardDurations(f)
		_ = f.Close()
		for label, perShard := range durations {
			out := merged[label]
			if out == nil {
				out = map[int64]int64{}
				merged[label] = out
			}
			for shard, ms := range perShard {
				if ms > out[shard] {
					out[shard] = ms
				}
			}
		}
	}

	results := Evaluate(merged)
	failed := false
	for _, im := range results {
		switch {
		case im.Fail() && opts.failOnImbalance:
			failed = true
			_, _ = fmt.Fprintf(stdout, "::error title=shard budget::%s\n", im.Message())
		case im.Warn():
			_, _ = fmt.Fprintf(stdout, "::warning title=shard budget::%s\n", im.Message())
		default:
			_, _ = fmt.Fprintf(stdout, "ok: %s\n", im.Message())
		}
	}
	if len(results) == 0 {
		_, _ = fmt.Fprintln(stdout, "shard budget: no sharded test target executed two or more shards in this invocation")
	}

	if failed {
		return 1
	}
	return 0
}
