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
// the honest floor: this run's shape cannot attribute time to gc.
func (b *BDTrace) GCSide(wall time.Duration) time.Duration {
	b.t.Helper()
	gcSide := wall - time.Duration(b.ChildMillis())*time.Millisecond
	if gcSide < 0 {
		return 0
	}
	return gcSide
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
