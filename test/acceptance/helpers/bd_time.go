package acceptancehelpers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// BDTraceEnv is the variable internal/beads.TraceBDCall reads to decide where
// to append its JSONL record. It is the JSON-format trace, deliberately a
// different variable from the older line-format GC_BD_TRACE.
const BDTraceEnv = "GC_BD_TRACE_JSON"

// BDTraceRecord is the part of one traced bd invocation an acceptance gate
// reads. TraceBDCall writes more fields (callers, scope, tick trigger, pids);
// they are deliberately not decoded here, so a new field upstream cannot break
// a measurement that never wanted it.
type BDTraceRecord struct {
	// Source is the call site's tag, e.g. "go:gc-bd-passthrough" for the
	// `gc bd ...` exec.
	Source string `json:"source"`
	// Args is the bd argv, without argv[0].
	Args []string `json:"args"`
	// DurMs is how long the bd child took, measured around the exec by the
	// process that spawned it.
	DurMs int64 `json:"dur_ms"`
	// ExitCode is the child's status.
	ExitCode int `json:"exit_code"`
}

// BDTrace is a per-test GC_BD_TRACE_JSON sink.
//
// It exists to make ONE quantity computable that neither a stopwatch nor the
// RecordingBD fork census can produce on its own: gc's own share of a command's
// wall time.
//
// The fork census answers "how many bd children", which is the deterministic
// gate. It cannot answer "was gc fast", because a passthrough command like
// `gc bd list --json` is one exec by construction — no store split can remove
// it — and its wall time is therefore bd's time plus gc's, with bd's part
// varying by machine, by database size and by whether the proxy was warm. A
// wall-clock assertion on that total would be a flake generator that fails on
// a loaded CI box and passes on a fast one, measuring the wrong process either
// way. Subtracting the traced child time leaves the part gc is answerable for.
type BDTrace struct {
	// Path is the trace file, to be exported as GC_BD_TRACE_JSON to every
	// process under test.
	Path string

	t *testing.T
}

// NewBDTrace returns a trace sink in its own directory.
//
// The file is NOT created: TraceBDCall opens with O_CREATE, and an absent file
// is the honest reading of "nothing traced", which is a legitimate outcome for
// a step that forked no bd at all.
func NewBDTrace(t *testing.T) *BDTrace {
	t.Helper()
	dir := filepath.Join(TempDir(t), "bd-trace")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create bd trace directory: %v", err)
	}
	return &BDTrace{Path: filepath.Join(dir, "bd-trace.jsonl"), t: t}
}

// Records returns every traced bd invocation, oldest first.
//
// A line that does not decode fails the test rather than being skipped. The
// skip would be the dangerous direction: a dropped record under-reports child
// time, which over-reports gc-side time, which fails a budget gate with a
// number nobody can reproduce — or, if the gate is an upper bound on gc time,
// silently passes a gc that was slow.
func (b *BDTrace) Records() []BDTraceRecord {
	b.t.Helper()
	data, err := os.ReadFile(b.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		b.t.Fatalf("read bd trace %s: %v", b.Path, err)
	}
	records, parseErr := parseBDTrace(data)
	if parseErr != nil {
		b.t.Fatalf("bd trace %s: %v", b.Path, parseErr)
	}
	return records
}

// parseBDTrace decodes the JSONL sink. It is separated from Records so the
// refusal above has a test that does not have to fail one.
func parseBDTrace(data []byte) ([]BDTraceRecord, error) {
	var out []BDTraceRecord
	for i, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record BDTraceRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			return nil, fmt.Errorf("line %d is not a trace record (%w): a dropped record under-reports bd child time, so this run's gc-side budget cannot be trusted", i+1, err)
		}
		out = append(out, record)
	}
	return out, nil
}

// ChildMillis is the summed traced bd-child time for everything in the trace.
func (b *BDTrace) ChildMillis() int64 {
	b.t.Helper()
	var total int64
	for _, record := range b.Records() {
		total += record.DurMs
	}
	return total
}

// GCSide returns wall minus the traced bd-child time: the part of a command's
// duration gc itself is answerable for.
//
// It clamps at zero rather than reporting a negative budget. Children are timed
// by the process that spawned them, so concurrent forks sum to more than the
// wall clock they ran in — and a negative number read as "gc took less than no
// time" would pass every upper-bound assertion ever written against it. Zero is
// the honest floor: this run's shape cannot attribute time to gc. It is a
// REPORT's floor, not a gate's: under an upper bound zero passes, so a budget
// gate uses PassthroughGCSide, which refuses the shape instead.
func (b *BDTrace) GCSide(wall time.Duration) time.Duration {
	b.t.Helper()
	gcSide := wall - time.Duration(b.ChildMillis())*time.Millisecond
	if gcSide < 0 {
		return 0
	}
	return gcSide
}

// BDPassthroughTraceSource is the tag cmd/gc/cmd_bd.go traces the `gc bd ...`
// passthrough exec under.
const BDPassthroughTraceSource = "go:gc-bd-passthrough"

// PassthroughGCSide is one sample's gc-side time for a `gc bd ...` passthrough:
// wall minus the summed traced bd-child time — refusing, instead of clamping,
// the two shapes in which that difference is not gc's side at all (round3
// review, completeness).
//
// GCSide clamps a negative difference to zero, which is honest for a report
// and wrong for a gate: under an upper bound, zero PASSES, so a trace that
// over-reported child time (children that overlapped, or records that were
// not this command's) turned the gc-side budget into one that passed whatever
// gc bolted onto the passthrough. A passthrough's children run one after
// another inside gc's own process lifetime, so their sum cannot exceed the
// wall clock around that process; when it does, the sample is not measuring
// this command. And a trace with no passthrough record is not a sample of the
// passthrough at all.
func PassthroughGCSide(records []BDTraceRecord, wall time.Duration) (time.Duration, error) {
	var child time.Duration
	passthrough := false
	for _, record := range records {
		child += time.Duration(record.DurMs) * time.Millisecond
		if record.Source == BDPassthroughTraceSource {
			passthrough = true
		}
	}
	if !passthrough {
		return 0, fmt.Errorf("the trace has no %s record among %d: this sample did not measure the passthrough", BDPassthroughTraceSource, len(records))
	}
	if child > wall {
		return 0, fmt.Errorf("the traced bd children sum to %s, past the %s the whole command took: they overlapped or are not this command's, so gc's side cannot be attributed", child, wall)
	}
	return wall - child, nil
}

// MarginalOverFloor is a command's gc-side time over the bare-process floor
// measured beside it, and refuses a floor that is not one.
//
// The floor (`gc --help`) does strictly less than any command, so a small
// inversion is noise and reads as a zero marginal — which is also the truth,
// since the command's gc side is then within noise of starting a process at
// all. An inversion larger than the budget itself is not noise: the floor has
// stopped measuring "start gc", and a clamped zero would pass any gc side up
// to that inflated floor.
func MarginalOverFloor(gcSide, floor, budget time.Duration) (time.Duration, error) {
	if floor > gcSide+budget {
		return 0, fmt.Errorf("the process floor (%s) exceeds the command's own gc side (%s) by more than the %s budget: the floor is not measuring a bare gc start, and a zero marginal would pass anything under it", floor, gcSide, budget)
	}
	if marginal := gcSide - floor; marginal > 0 {
		return marginal, nil
	}
	return 0, nil
}

// Reset discards the trace so a measurement can be attributed to one step of a
// test rather than to everything that ran before it — the same contract as
// RecordingBD.Reset, and for the same reason.
func (b *BDTrace) Reset() {
	b.t.Helper()
	if err := os.Remove(b.Path); err != nil && !os.IsNotExist(err) {
		b.t.Fatalf("reset bd trace: %v", err)
	}
}

// Describe renders the trace for a failure message, so a budget assertion says
// which bd calls made up the time it subtracted.
func (b *BDTrace) Describe() string {
	records := b.Records()
	if len(records) == 0 {
		return "(no traced bd calls)"
	}
	lines := make([]string, 0, len(records))
	for _, record := range records {
		lines = append(lines, fmt.Sprintf("  %s exit=%d %dms: bd %s",
			record.Source, record.ExitCode, record.DurMs, strings.Join(record.Args, " ")))
	}
	return strings.Join(lines, "\n")
}
