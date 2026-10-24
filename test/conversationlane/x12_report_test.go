//go:build integration

package conversationlane_test

// The X12 report: the pass lines P1 to P5, I4, I8, R1, R3, the load's
// succeeds and budget lines, and harness, with their measured values and
// offending items, and the measurements R1 to R3, as JSON and as the
// Markdown table the PR body carries. Every pass line is asserted in the
// test that measures it; the report only aggregates, says
// "not run" for a line nothing evaluated, and is stamped FAILED when the
// test binary failed.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/opjournal"
)

// x12Collector gathers what the X12 tests measured.
type x12Collector struct {
	mu      sync.Mutex
	started time.Time
	// gcBin is the test-hooks gc the cities ran, for the report's version.
	gcBin    string
	cells    []x12CellRecord
	problems []x12ProblemRecord
	loads    []x12LoadRecord
	arms     []x12ArmRecord
	// evals counts, per pass line, the items checks evaluated under it.
	evals map[string]int
}

// x12Report is the test binary's collector.
var x12Report = &x12Collector{started: time.Now(), evals: map[string]int{}}

// eval counts n items evaluated under line.
func (r *x12Collector) eval(line string, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evals[line] += n
}

// x12P5Identifiers is the eval line of P5's identifier scan: every
// delivery a city saw, scanned for op identifiers. It is counted apart from
// "P5", the deliveries compared with off's text, so a delivery both checks
// see is not counted twice.
const x12P5Identifiers = "P5 identifiers"

// x12LineBudget is the pass line of the evidence load's wall-time budget;
// x12LineSucceeds the fault-free load's: every well-formed request of it
// succeeds. passLines judges both from the load records' data (Budget and
// Elapsed, NotSucceeded), not from the problems the load's test wrote.
const (
	x12LineBudget   = "budget"
	x12LineSucceeds = "succeeds"
)

// x12CellRecord is one matrix cell in the report.
type x12CellRecord struct {
	Tier         string         `json:"tier"`
	Backend      string         `json:"backend"`
	Mode         string         `json:"mode"`
	Cell         string         `json:"cell"`
	Surface      string         `json:"surface"`
	Original     string         `json:"original"`
	OrigOut      string         `json:"original_outcome,omitempty"`
	Retry        string         `json:"retry,omitempty"`
	RetryOut     string         `json:"retry_outcome,omitempty"`
	Rerun        string         `json:"rerun_after_finish_by,omitempty"`
	Op           string         `json:"op"`
	Outcome      string         `json:"outcome"`
	Closer       string         `json:"closer"`
	Attempt      int            `json:"attempt"`
	ExpectedExec int            `json:"expected_executions"`
	ObservedExec int            `json:"observed_executions"`
	Deliveries   int            `json:"deliveries"`
	Effects      map[string]int `json:"runtime_effects,omitempty"`
	Problems     []string       `json:"problems,omitempty"`
}

// x12ProblemRecord is a failed check outside a cell.
type x12ProblemRecord struct {
	Tier, Backend, Mode, Line, Text string
}

// x12LoadRecord is one backend's fault-free load.
type x12LoadRecord struct {
	Tier    string                    `json:"tier"`
	Backend string                    `json:"backend"`
	P4      x12P4                     `json:"p4"`
	R1      map[string]map[string]int `json:"r1_refusals"`
	R2      []x12Latency              `json:"r2_latency"`
	R3      x12Growth                 `json:"r3_growth"`
	// Budget is the evidence load's wall-time budget (0 in the gating
	// tier); Elapsed is how long its rounds took, or, for a load that
	// stopped before its last round (Finished false), how far it got.
	Budget   time.Duration `json:"budget,omitempty"`
	Elapsed  time.Duration `json:"elapsed"`
	Finished bool          `json:"finished"`
	// WellFormed counts the load's well-formed requests; NotSucceeded
	// names each of them that did not succeed (x12NotSucceeded).
	WellFormed   int      `json:"well_formed_requests"`
	NotSucceeded []string `json:"not_succeeded,omitempty"`
	Problem      []string `json:"problems,omitempty"`
}

// x12P4 is P4's measure.
type x12P4 struct {
	Requests  int      `json:"requests"`
	Equal     int      `json:"equal"`
	Late      []string `json:"late_mismatches,omitempty"`
	Other     []string `json:"other_mismatches,omitempty"`
	CodeDiffs []string `json:"code_differences,omitempty"`
	NotReplay int      `json:"not_replayed_open"`
	// OpenUnexplained are open ops no retryable error explains.
	OpenUnexplained []string `json:"open_unexplained,omitempty"`
	// MailNotifyExcluded counts mail notifies, outside P4's share: no
	// verb replays them.
	MailNotifyExcluded int     `json:"mail_notify_excluded"`
	EqualShare         float64 `json:"equal_share"`
}

// x12Latency is one R2 row.
type x12Latency struct {
	Backend string        `json:"backend"`
	Mode    string        `json:"mode"`
	Path    string        `json:"path"`
	N       int           `json:"n"`
	P50     time.Duration `json:"p50"`
	P95     time.Duration `json:"p95"`
	P99     time.Duration `json:"p99"`
	Max     time.Duration `json:"max"`
}

// x12Growth is R3.
type x12Growth struct {
	Recorded      int                `json:"recorded_requests"`
	NewRows       int                `json:"new_op_rows"`
	LogicalByKind map[string]x12Size `json:"logical_bytes_by_kind"`
	// Rounds is each round's physical growth; PhysicalBytes the recorded
	// rounds' sum, PhysicalOverOff that less each one's preceding off round,
	// and PhysicalPerOp PhysicalOverOff over NewRows.
	// PhysicalPerOp is absent when no figure per op can be stated;
	// PhysicalNote says why, or states the figure.
	Rounds          []x12RoundGrowth `json:"rounds"`
	PhysicalBytes   int64            `json:"physical_bytes_recorded_rounds"`
	PhysicalOverOff int64            `json:"physical_bytes_over_off"`
	PhysicalPerOp   *float64         `json:"physical_bytes_per_op,omitempty"`
	PhysicalNote    string           `json:"physical_note"`
	DailyOps        int              `json:"daily_ops_input,omitempty"`
	DailyBytes      float64          `json:"daily_bytes,omitempty"`
	DailyFormulaMD  string           `json:"daily_formula"`
}

// x12Size is a size distribution.
type x12Size struct {
	N    int     `json:"n"`
	Mean float64 `json:"mean"`
	P95  int     `json:"p95"`
}

// x12ArmRecord is one incapable-store arm's result.
type x12ArmRecord struct {
	Backend  string   `json:"backend"`
	Mode     string   `json:"mode"`
	Requests int      `json:"requests"`
	Problems []string `json:"problems,omitempty"`
}

// setGC records the gc binary the cities run.
func (r *x12Collector) setGC(bin string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gcBin = bin
}

func (r *x12Collector) addCell(cell *x12Cell) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := x12CellRecord{
		Tier: cell.tier, Backend: cell.backend, Mode: cell.mode, Cell: cell.spec.name(), Surface: cell.spec.verb.surface,
		Original: cell.orig.String(), Op: cell.opID, Outcome: cell.finalOutcome, Closer: cell.closer,
		Attempt: cell.op.Attempt, ExpectedExec: cell.expectedExec, ObservedExec: cell.observedExec, Deliveries: cell.deliveries, Effects: cell.effects,
	}
	if cell.origOut.state != "" {
		rec.OrigOut = cell.origOut.state
	}
	if cell.spec.retry {
		rec.Retry = cell.retry.String()
		rec.RetryOut = cell.retryOut.state
	}
	if cell.evidenceRerun != nil {
		rec.Rerun = cell.evidenceRerun.String()
	}
	for _, p := range cell.problems {
		rec.Problems = append(rec.Problems, "["+p.line+"] "+p.text)
	}
	r.cells = append(r.cells, rec)
}

func (r *x12Collector) problem(tier, backend, mode, line, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.problems = append(r.problems, x12ProblemRecord{Tier: tier, Backend: backend, Mode: mode, Line: line, Text: text})
}

func (r *x12Collector) addLoad(l x12LoadRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loads = append(r.loads, l)
}

func (r *x12Collector) addArm(a x12ArmRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.arms = append(r.arms, a)
}

func (r *x12Collector) empty() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cells) == 0 && len(r.loads) == 0 && len(r.arms) == 0 && len(r.problems) == 0
}

// x12PassLine is one pass line's verdict.
type x12PassLine struct {
	Line      string   `json:"line"`
	Wording   string   `json:"wording"`
	Status    string   `json:"status"`
	Measured  string   `json:"measured"`
	Threshold string   `json:"threshold"`
	Offending []string `json:"offending,omitempty"`
}

// x12ReportDoc is the JSON report.
type x12ReportDoc struct {
	GCVersion string `json:"gc_version"`
	// Result is "passed", or FAILED with the test binary's exit code: a
	// failed run's pass lines are not evidence.
	Result    string             `json:"result"`
	Tier      string             `json:"tier"`
	Backend   string             `json:"backend"`
	Started   time.Time          `json:"started"`
	WallTime  string             `json:"wall_time"`
	PassLines []x12PassLine      `json:"pass_lines"`
	Cells     []x12CellRecord    `json:"cells"`
	Loads     []x12LoadRecord    `json:"loads"`
	Arms      []x12ArmRecord     `json:"incapable_arms"`
	Problems  []x12ProblemRecord `json:"other_problems,omitempty"`
}

// passLines aggregates every recorded problem under its pass line.
func (r *x12Collector) passLines() []x12PassLine {
	offending := map[string][]string{}
	for _, c := range r.cells {
		for _, p := range c.Problems {
			line := strings.TrimPrefix(strings.SplitN(p, "]", 2)[0], "[")
			offending[line] = append(offending[line], c.Backend+"/"+c.Mode+"/"+c.Cell+": "+p)
		}
	}
	for _, p := range r.problems {
		offending[p.Line] = append(offending[p.Line], p.Backend+"/"+p.Mode+": "+p.Text)
	}
	for _, l := range r.loads {
		for _, p := range l.Problem {
			line := strings.TrimPrefix(strings.SplitN(p, "]", 2)[0], "[")
			if line == x12LineBudget || line == x12LineSucceeds {
				// Judged below from the record's data.
				continue
			}
			offending[line] = append(offending[line], l.Backend+": "+p)
		}
		if p := x12BudgetProblem(l.Budget, l.Elapsed); p != "" {
			offending[x12LineBudget] = append(offending[x12LineBudget], l.Backend+": "+p)
		}
		if l.Tier == "evidence" && l.Budget <= 0 {
			offending["harness"] = append(offending["harness"], l.Backend+": the evidence load carries no wall-time budget")
		}
		for _, n := range l.NotSucceeded {
			offending[x12LineSucceeds] = append(offending[x12LineSucceeds], l.Backend+": "+n)
		}
	}
	for _, a := range r.arms {
		for _, p := range a.Problems {
			offending["I8"] = append(offending["I8"], a.Backend+"/"+a.Mode+": "+p)
		}
	}
	// The load lines count what the load records carry. A load that stopped
	// before its last round has not shown it finishes within its budget; it
	// fails the line only once it ran past it.
	evals := maps.Clone(r.evals)
	for _, l := range r.loads {
		if l.Budget > 0 && l.Finished {
			evals[x12LineBudget]++
		}
		evals[x12LineSucceeds] += l.WellFormed
	}
	verdict := func(line string) string {
		switch {
		case len(offending[line]) > 0:
			return "FAIL"
		case evals[line] == 0 && (line != "P5" || evals[x12P5Identifiers] == 0):
			return "not run"
		}
		return "pass"
	}
	measured := func(line string) string {
		return fmt.Sprintf("%d offending of %d evaluated", len(offending[line]), evals[line])
	}
	p4 := measured("P4")
	for _, l := range r.loads {
		p4 = fmt.Sprintf("%s: %d/%d equal (%.4f), %d late, %d other, %d open unexplained", l.Backend, l.P4.Equal, l.P4.Requests, l.P4.EqualShare, len(l.P4.Late), len(l.P4.Other), len(l.P4.OpenUnexplained))
	}
	budget := measured(x12LineBudget)
	for _, l := range r.loads {
		switch {
		case l.Budget <= 0:
		case l.Finished:
			budget = fmt.Sprintf("%s: %s of %s", l.Backend, l.Elapsed, l.Budget)
		default:
			budget = fmt.Sprintf("%s: stopped before its last round after %s of %s", l.Backend, l.Elapsed, l.Budget)
		}
	}
	p5 := fmt.Sprintf("%d offending; %d deliveries compared with off's text, %d scanned for identifiers", len(offending["P5"]), r.evals["P5"], r.evals[x12P5Identifiers])
	p2 := fmt.Sprintf("%d offending; %d ops checked against finish_by, %d cells' immediate outcomes", len(offending["P2"]), r.evals["P2"], r.evals["P2 immediate"])
	lines := []x12PassLine{
		{Line: "P1", Wording: "require has 0 answered requests without an op", Threshold: "0", Measured: measured("P1"), Status: verdict("P1")},
		{Line: "P2", Wording: "every op has exactly one outcome within finish_by + 60 s", Threshold: "0", Measured: p2, Status: verdict("P2")},
		{Line: "P3", Wording: "executions as expected, duplicates only at K3 (and only after a retry)", Threshold: "0 outside API {K3, K5} with a retry", Measured: measured("P3"), Status: verdict("P3")},
		{Line: "P4", Wording: "projections equal the client-visible results in >= 99.9% of fault-free requests", Threshold: ">= 0.999, 0 non-late mismatches, 0 unexplained open ops", Measured: p4, Status: verdict("P4")},
		{Line: "P5", Wording: "delivered text equal to the off text in every arm, adopted re-executions included (I11)", Threshold: "0", Measured: p5, Status: verdict("P5")},
		{Line: "I4", Wording: "one op per key", Threshold: "0", Measured: measured("I4"), Status: verdict("I4")},
		{Line: "I8", Wording: "an incapable store degrades as off in auto and refuses in require", Threshold: "0", Measured: measured("I8"), Status: verdict("I8")},
		{Line: "R1", Wording: "every well-formed recorded-mode request records; each provoked cause lands in its class", Threshold: "0", Measured: measured("R1"), Status: verdict("R1")},
		{Line: "R3", Wording: "the store gains one op row per recorded request", Threshold: "0", Measured: measured("R3"), Status: verdict("R3")},
		{Line: x12LineSucceeds, Wording: "every well-formed request of the fault-free load succeeds, off rounds included", Threshold: "0", Measured: measured(x12LineSucceeds), Status: verdict(x12LineSucceeds)},
		{Line: x12LineBudget, Wording: "the evidence load finishes within its wall-time budget", Threshold: "elapsed <= budget", Measured: budget, Status: verdict(x12LineBudget)},
		{Line: "harness", Wording: "every failpoint fired as armed", Threshold: "0", Measured: measured("harness"), Status: verdict("harness")},
	}
	for i := range lines {
		lines[i].Offending = offending[lines[i].Line]
	}
	return lines
}

// writeX12Report writes the report when X12 recorded anything; code is the
// test binary's exit code.
func writeX12Report(code int) {
	if x12Report.empty() {
		return
	}
	r := x12Report
	r.mu.Lock()
	defer r.mu.Unlock()
	tier, backend := "gating", "sqlite"
	if x12Evidence() {
		tier, backend = "evidence", os.Getenv(x12BackendEnv)
	}
	doc := x12ReportDoc{
		GCVersion: gcVersion(r.gcBin), Result: x12Result(code), Tier: tier, Backend: backend, Started: r.started,
		WallTime:  time.Since(r.started).Round(time.Second).String(),
		PassLines: r.passLines(), Cells: r.cells, Loads: r.loads, Arms: r.arms, Problems: r.problems,
	}
	sort.Slice(doc.Cells, func(i, j int) bool {
		a, b := doc.Cells[i], doc.Cells[j]
		return a.Backend+a.Mode+a.Cell < b.Backend+b.Mode+b.Cell
	})
	dir := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR")
	if dir == "" {
		var err error
		if dir, err = os.MkdirTemp("", "x12-report"); err != nil {
			fmt.Fprintf(os.Stderr, "x12: report dir: %v\n", err) //nolint:errcheck // best-effort
			return
		}
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "x12: encoding the report: %v\n", err) //nolint:errcheck // best-effort
		return
	}
	name := "x12-report-" + backend
	if part := os.Getenv(x12PartEnv); part != "" {
		name += "-" + part
	}
	jsonPath := filepath.Join(dir, name+".json")
	mdPath := filepath.Join(dir, name+".md")
	md := renderX12Markdown(doc)
	for path, body := range map[string][]byte{jsonPath: data, mdPath: []byte(md)} {
		if err := os.WriteFile(path, body, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "x12: writing %s: %v\n", path, err) //nolint:errcheck // best-effort
		}
	}
	fmt.Fprintf(os.Stderr, "x12: report written to %s and %s\n", jsonPath, mdPath) //nolint:errcheck // best-effort
	fmt.Print(md)
}

// x12Result is the report's result for the test binary's exit code.
func x12Result(code int) string {
	if code != 0 {
		return fmt.Sprintf("FAILED: the test binary exited %d; no pass line of this run is evidence", code)
	}
	return "passed"
}

// x12SourceEnv names the source revision an evidence run was built from
// (BUILD.bazel passes it with --test_env): Bazel's test-hooks gc is not
// stamped, so its own version says only "dev".
const x12SourceEnv = "GC_X12_SOURCE"

// gcVersion identifies the gc the cities ran: its gc version --long line,
// the sha256 of the binary, and the source revision the run names.
func gcVersion(bin string) string {
	if bin == "" {
		return "unknown: no X12 city ran"
	}
	var version string
	if out, err := exec.Command(bin, "version", "--long").CombinedOutput(); err != nil {
		version = "unknown: " + err.Error()
	} else {
		version = strings.Join(strings.Fields(string(out)), " ")
	}
	sum := "unreadable"
	if data, err := os.ReadFile(bin); err == nil {
		sum = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	source := os.Getenv(x12SourceEnv)
	if source == "" {
		source = "not given (" + x12SourceEnv + ")"
	}
	return fmt.Sprintf("%s; binary sha256 %s; source %s", version, sum, source)
}

// renderX12Markdown is the PR-body table.
func renderX12Markdown(doc x12ReportDoc) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## X12 report (%s tier, %s)\n\n", doc.Tier, doc.Backend)
	fmt.Fprintf(&b, "**Result: %s**\n\n", doc.Result)
	fmt.Fprintf(&b, "gc: `%s`; started %s; wall time %s; %d cells\n\n", doc.GCVersion, doc.Started.UTC().Format(time.RFC3339), doc.WallTime, len(doc.Cells))
	b.WriteString("| Line | Wording | Threshold | Measured | Status |\n|---|---|---|---|---|\n")
	for _, l := range doc.PassLines {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", l.Line, l.Wording, l.Threshold, l.Measured, l.Status)
	}
	for _, l := range doc.PassLines {
		if len(l.Offending) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n### %s offending items\n\n", l.Line)
		for _, o := range l.Offending {
			fmt.Fprintf(&b, "- %s\n", o)
		}
	}
	for _, l := range doc.Loads {
		fmt.Fprintf(&b, "\n### Fault-free load (%s)\n\n", l.Backend)
		switch {
		case !l.Finished:
			fmt.Fprintf(&b, "Load stopped before its last round after %s\n\n", l.Elapsed)
		case l.Budget > 0:
			fmt.Fprintf(&b, "Load wall time %s of its %s budget\n\n", l.Elapsed, l.Budget)
		default:
			fmt.Fprintf(&b, "Load wall time %s\n\n", l.Elapsed)
		}
		fmt.Fprintf(&b, "P4: %d requests, %d equal (%.4f); late mismatches %d; other %d; open ops not replayed %d (%d unexplained); mail notifies excluded %d\n\n", l.P4.Requests, l.P4.Equal, l.P4.EqualShare, len(l.P4.Late), len(l.P4.Other), l.P4.NotReplay, len(l.P4.OpenUnexplained), l.P4.MailNotifyExcluded)
		for _, d := range l.P4.CodeDiffs {
			fmt.Fprintf(&b, "- code difference: %s\n", d)
		}
		fmt.Fprintf(&b, "\nWell-formed requests that did not succeed: %d of %d\n", len(l.NotSucceeded), l.WellFormed)
		for _, n := range l.NotSucceeded {
			fmt.Fprintf(&b, "- %s\n", n)
		}
		b.WriteString("\nR1 refusals and unrecorded requests:\n\n| Mode/surface | Class | Count |\n|---|---|---|\n")
		for _, k := range sortedKeys(l.R1) {
			for _, c := range sortedKeys(l.R1[k]) {
				fmt.Fprintf(&b, "| %s | %s | %d |\n", k, c, l.R1[k][c])
			}
		}
		b.WriteString("\nR2 latency (reported, not asserted):\n\n| Backend | Mode | Path | n | p50 | p95 | p99 | max |\n|---|---|---|---|---|---|---|---|\n")
		for _, r := range l.R2 {
			fmt.Fprintf(&b, "| %s | %s | %s | %d | %s | %s | %s | %s |\n", r.Backend, r.Mode, r.Path, r.N, r.P50, r.P95, r.P99, r.Max)
		}
		g := l.R3
		fmt.Fprintf(&b, "\nR3 op-row growth (%s): %d recorded requests, the store gained %d op rows; physical %d bytes over the recorded rounds, %d over their off rounds; %s; %s\n\n", l.Backend, g.Recorded, g.NewRows, g.PhysicalBytes, g.PhysicalOverOff, g.PhysicalNote, g.DailyFormulaMD)
		b.WriteString("| Kind | n | mean logical bytes | p95 |\n|---|---|---|---|\n")
		for _, k := range sortedKeys(g.LogicalByKind) {
			s := g.LogicalByKind[k]
			fmt.Fprintf(&b, "| %s | %d | %.0f | %d |\n", k, s.N, s.Mean, s.P95)
		}
	}
	if len(doc.Arms) > 0 {
		b.WriteString("\n### Incapable store\n\n| Backend | Mode | Requests | Problems |\n|---|---|---|---|\n")
		for _, a := range doc.Arms {
			fmt.Fprintf(&b, "| %s | %s | %d | %d |\n", a.Backend, a.Mode, a.Requests, len(a.Problems))
		}
	}
	b.WriteString("\n### Cells\n\n| Backend/mode | Cell | Original | Retry | Op outcome | Closer | Attempt | Executions (expected/observed) | Deliveries | Effects |\n|---|---|---|---|---|---|---|---|---|---|\n")
	for _, c := range doc.Cells {
		fmt.Fprintf(&b, "| %s/%s | %s | %s | %s | %s | %s | %d | %d/%d | %d | %v |\n", c.Backend, c.Mode, c.Cell,
			mdCell(c.Original), mdCell(c.Retry), c.Outcome, c.Closer, c.Attempt, c.ExpectedExec, c.ObservedExec, c.Deliveries, c.Effects)
	}
	return b.String()
}

// mdCell shortens an answer for a table cell.
func mdCell(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "|", "/"), "\n", " ")
	if len(s) > 60 {
		s = s[:60] + "…"
	}
	return s
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestX12PassLineP5CountsEachCheckOnce pins P5's measured counts: the text
// comparison and the identifier scan each count a delivery once, under their
// own figure, and either alone makes the line run.
func TestX12PassLineP5CountsEachCheckOnce(t *testing.T) {
	p5 := func(r *x12Collector) x12PassLine {
		for _, l := range r.passLines() {
			if l.Line == "P5" {
				return l
			}
		}
		t.Fatal("no P5 pass line")
		return x12PassLine{}
	}
	both := &x12Collector{evals: map[string]int{}}
	both.eval("P5", 92)
	both.eval(x12P5Identifiers, 92)
	if got, want := p5(both).Measured, "0 offending; 92 deliveries compared with off's text, 92 scanned for identifiers"; got != want {
		t.Errorf("P5 measured %q, want %q", got, want)
	}
	if got := p5(both).Status; got != "pass" {
		t.Errorf("P5 status %q, want pass", got)
	}
	scanOnly := &x12Collector{evals: map[string]int{}}
	scanOnly.eval(x12P5Identifiers, 4)
	if got := p5(scanOnly).Status; got != "pass" {
		t.Errorf("P5 status with only the identifier scan %q, want pass", got)
	}
	if got := p5(&x12Collector{evals: map[string]int{}}).Status; got != "not run" {
		t.Errorf("P5 status with nothing evaluated %q, want not run", got)
	}
}

// TestX12LifecycleWaitIsBoundedInTheGatingTier pins the P3 lifecycle wait:
// the gating tier fails a missing execution at x12EffectWait; only the
// evidence tier waits x12StateWait.
func TestX12LifecycleWaitIsBoundedInTheGatingTier(t *testing.T) {
	t.Setenv(x12EvidenceEnv, "")
	if got := x12LifecycleWait(); got != x12EffectWait {
		t.Errorf("gating tier lifecycle wait %s, want %s", got, x12EffectWait)
	}
	t.Setenv(x12EvidenceEnv, "1")
	if got := x12LifecycleWait(); got != x12StateWait {
		t.Errorf("evidence tier lifecycle wait %s, want %s", got, x12StateWait)
	}
}

// TestX12ArmsByPart pins which store arms each evidence part runs on
// (x12PartArms): the crash arms on SQLite and embedded Dolt, and the load on
// SQLite and on a bd-provisioned Dolt sql-server, served natively, which
// stands in for the BdStore-served arm the maintainers' 2026-10-23 ruling on
// #8217 asked for until a maintainer accepts it or ga-ve3tjw adds that arm.
func TestX12ArmsByPart(t *testing.T) {
	for _, tc := range []struct {
		part, backend string
		want          storeArm
	}{
		{x12PartMatrix, "sqlite", armSQLite},
		{x12PartMatrix, "dolt", armDolt},
		{x12PartLoad, "sqlite", armSQLite},
		{x12PartLoad, "bd-dolt", armBdDolt},
	} {
		got, err := x12ArmFor(tc.part, tc.backend)
		if err != nil || got != tc.want {
			t.Errorf("x12ArmFor(%q, %q) = %q, %v; want %q", tc.part, tc.backend, got, err, tc.want)
		}
	}
	for _, tc := range []struct{ part, backend string }{
		{x12PartLoad, "dolt"},
		{x12PartMatrix, "bd-dolt"},
		{x12PartLoad, "doltlite"},
		{"", "sqlite"},
	} {
		if got, err := x12ArmFor(tc.part, tc.backend); err == nil {
			t.Errorf("x12ArmFor(%q, %q) = %q, want an error", tc.part, tc.backend, got)
		}
	}
}

// TestX12LoadRounds pins the load's rounds: six (off, auto, require,
// twice) on every evidence arm, the bd-dolt one included, three in the
// gating tier.
func TestX12LoadRounds(t *testing.T) {
	six := []string{"off", "auto", "require", "off", "auto", "require"}
	if got := x12LoadModes(true); !slices.Equal(got, six) {
		t.Errorf("evidence rounds = %v, want %v", got, six)
	}
	if got := x12LoadModes(false); !slices.Equal(got, six[:3]) {
		t.Errorf("gating rounds = %v, want %v", got, six[:3])
	}
}

// TestX12BackendFollowsTheTestsPart pins that a test runs on its own part's
// arm only: outside the evidence tier on SQLite whatever the variables say,
// in it on the variant's backend when the variant is the test's part, and
// refused when another part's variant runs it (a widened -test.run).
func TestX12BackendFollowsTheTestsPart(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	for _, tc := range []struct {
		name   string
		getenv func(string) string
		part   string
		want   storeArm
	}{
		{"gating", env(x12PartEnv, x12PartLoad, x12BackendEnv, "bd-dolt"), x12PartMatrix, armSQLite},
		{"load variant, load test", env(x12EvidenceEnv, "1", x12PartEnv, x12PartLoad, x12BackendEnv, "bd-dolt"), x12PartLoad, armBdDolt},
		{"matrix variant, matrix test", env(x12EvidenceEnv, "1", x12PartEnv, x12PartMatrix, x12BackendEnv, "dolt"), x12PartMatrix, armDolt},
	} {
		got, err := x12BackendFrom(tc.getenv, tc.part)
		if err != nil || got != tc.want {
			t.Errorf("%s: x12BackendFrom(%s) = %q, %v; want %q", tc.name, tc.part, got, err, tc.want)
		}
	}
	for _, tc := range []struct {
		name   string
		getenv func(string) string
		part   string
	}{
		{"load variant, matrix test", env(x12EvidenceEnv, "1", x12PartEnv, x12PartLoad, x12BackendEnv, "bd-dolt"), x12PartMatrix},
		{"matrix variant, load test", env(x12EvidenceEnv, "1", x12PartEnv, x12PartMatrix, x12BackendEnv, "sqlite"), x12PartLoad},
		{"no part", env(x12EvidenceEnv, "1", x12BackendEnv, "sqlite"), x12PartLoad},
		{"load test, matrix-only arm", env(x12EvidenceEnv, "1", x12PartEnv, x12PartLoad, x12BackendEnv, "dolt"), x12PartLoad},
	} {
		if got, err := x12BackendFrom(tc.getenv, tc.part); err == nil {
			t.Errorf("%s: x12BackendFrom(%s) = %q, want an error", tc.name, tc.part, got)
		}
	}
}

// TestX12BudgetProblem pins the budget decision: a load within or at its
// budget keeps it, one past it breaks it, and the gating tier's zero budget
// is never broken.
func TestX12BudgetProblem(t *testing.T) {
	for _, tc := range []struct {
		budget, elapsed time.Duration
		broken          bool
	}{
		{90 * time.Minute, 63 * time.Minute, false},
		{90 * time.Minute, 90 * time.Minute, false},
		{90 * time.Minute, 90*time.Minute + time.Second, true},
		{0, 5 * time.Hour, false},
	} {
		got := x12BudgetProblem(tc.budget, tc.elapsed)
		if (got != "") != tc.broken {
			t.Errorf("x12BudgetProblem(%s, %s) = %q, want broken %v", tc.budget, tc.elapsed, got, tc.broken)
		}
	}
	if got, want := x12BudgetProblem(time.Minute, time.Hour), "the load took 1h0m0s, over its 1m0s budget"; got != want {
		t.Errorf("x12BudgetProblem = %q, want %q", got, want)
	}
}

// TestX12RoundsProblem pins that a load ran its tier's rounds: six on every
// evidence arm, three in the gating tier.
func TestX12RoundsProblem(t *testing.T) {
	rounds := func(modes ...string) []x12RoundGrowth {
		out := make([]x12RoundGrowth, len(modes))
		for i, m := range modes {
			out[i] = x12RoundGrowth{Mode: m}
		}
		return out
	}
	six := rounds("off", "auto", "require", "off", "auto", "require")
	if p := x12RoundsProblem("evidence", six); p != "" {
		t.Errorf("six evidence rounds: %s", p)
	}
	if p := x12RoundsProblem("gating", six[:3]); p != "" {
		t.Errorf("three gating rounds: %s", p)
	}
	if p := x12RoundsProblem("evidence", six[:3]); p == "" {
		t.Error("an evidence load of three rounds passed")
	}
	if p := x12RoundsProblem("evidence", six[:5]); p == "" {
		t.Error("an evidence load that stopped after five rounds passed")
	}
}

// TestX12PhysicalGrowth pins R3's physical measure: a figure per op only
// when every recorded round follows an off round, no round shrank, the store
// gained op rows and each recorded round grew no less than the off round
// before it.
func TestX12PhysicalGrowth(t *testing.T) {
	r := func(mode string, b int64) x12RoundGrowth { return x12RoundGrowth{Mode: mode, Bytes: b} }
	for _, tc := range []struct {
		name           string
		rounds         []x12RoundGrowth
		rows           int
		recorded, over int64
		perOp          float64
		stated         bool
		noteContains   string
	}{
		{"growth over off", []x12RoundGrowth{r("off", 100), r("auto", 300), r("require", 500), r("off", 100), r("auto", 300), r("require", 500)}, 8, 1600, 1200, 150, true, "150 physical bytes per op"},
		{"as much as off", []x12RoundGrowth{r("off", 100), r("auto", 100)}, 4, 100, 0, 0, true, "0 physical bytes per op"},
		{"a round compacted", []x12RoundGrowth{r("off", 30), r("auto", 88), r("require", 109), r("off", 51), r("auto", -164), r("require", 12)}, 10, 45, -117, 0, false, "round 5 (auto) shrank by 164 bytes"},
		{"an off round compacted", []x12RoundGrowth{r("off", -50), r("auto", 300)}, 3, 300, 350, 0, false, "round 1 (off) shrank by 50 bytes"},
		{"less than off", []x12RoundGrowth{r("off", 500), r("auto", 300)}, 3, 300, -200, 0, false, "round 2 (auto) grew 200 bytes less than the off round before it"},
		// The SQLite evidence load of 2026-10-24: the first off round
		// carried the WAL's one-off growth to its high-water mark, so the
		// total over off (433231 bytes, 411 per op) is what is left when
		// rounds of opposite sign cancel, not a measure of the ops.
		{"a round below its off round", []x12RoundGrowth{r("off", 4463081), r("auto", 2482417), r("require", 3100248), r("off", 557056), r("auto", 2408448), r("require", 2482392)}, 1054, 10473505, 433231, 0, false, "round 2 (auto) grew 1980664 bytes less than the off round before it"},
		{"one round below its off round", []x12RoundGrowth{r("off", 100), r("auto", 300), r("require", 500), r("off", 400), r("auto", 300), r("require", 900)}, 8, 2000, 1000, 0, false, "round 5 (auto) grew 100 bytes less than the off round before it"},
		{"no op rows", []x12RoundGrowth{r("off", 100), r("auto", 300)}, 0, 300, 200, 0, false, "gained no op rows"},
		{"no off round", []x12RoundGrowth{r("auto", 300), r("require", 300)}, 4, 600, 600, 0, false, "no off round before it"},
	} {
		got := x12PhysicalGrowth(tc.rounds, tc.rows)
		if got.RecordedBytes != tc.recorded || got.OverOff != tc.over {
			t.Errorf("%s: recorded %d, over off %d; want %d, %d", tc.name, got.RecordedBytes, got.OverOff, tc.recorded, tc.over)
		}
		if (got.PerOp != nil) != tc.stated || (got.PerOp != nil && *got.PerOp != tc.perOp) {
			perOp := "none"
			if got.PerOp != nil {
				perOp = strconv.FormatFloat(*got.PerOp, 'f', -1, 64)
			}
			t.Errorf("%s: per op %s, want stated %v (%v)", tc.name, perOp, tc.stated, tc.perOp)
		}
		if !strings.Contains(got.Note, tc.noteContains) {
			t.Errorf("%s: note %q, want it to say %q", tc.name, got.Note, tc.noteContains)
		}
	}
}

// TestX12BindingDirPerArm pins where R3 measures each arm's binding: the
// bd-dolt arm's data sits in its server's data dir, not in its workspace,
// which holds only bd's configuration.
func TestX12BindingDirPerArm(t *testing.T) {
	c := &laneCity{dir: "/city", doltServer: &doltServer{dataDir: "/root/dolt-data"}}
	for arm, want := range map[storeArm]string{
		armSQLite: "/city/.gc/lanes",
		armDolt:   "/city/.gc/storage/lanes/.beads",
		armBdDolt: "/root/dolt-data",
	} {
		c.arm = arm
		if got := c.bindingDir(); got != want {
			t.Errorf("%s bindingDir = %q, want %q", arm, got, want)
		}
	}
}

// TestX12NotSucceeded pins which load requests count as not succeeding: a
// well-formed one whose client saw no success, or whose closed op failed
// although its client saw success; never a provoked cause (R1's) or a
// skipped stop.
func TestX12NotSucceeded(t *testing.T) {
	submit, cliNudge, stop := verbByName("submit"), verbByName("cli-nudge-wait"), verbByName("stop")
	closed := func(arm opjournal.Arm, reason string) opjournal.Op {
		return opjournal.Op{ID: "gcg-op1", Status: opjournal.StatusClosed, Outcome: opjournal.Outcome{Arm: arm, Reason: reason}}
	}
	for _, tc := range []struct {
		name string
		r    x12LoadReq
		bad  bool
	}{
		{"delivered", x12LoadReq{mode: "auto", verb: submit, class: classDelivered, hasOp: true, op: closed(opjournal.ArmSucceeded, "")}, false},
		{"off, no op", x12LoadReq{mode: "off", verb: submit, class: classQueued}, false},
		{"stop skipped as not needed", x12LoadReq{mode: "require", verb: stop, class: classSucceeded, hasOp: true, op: closed(opjournal.ArmSkipped, opjournal.ReasonNotNeeded)}, false},
		{"provoked", x12LoadReq{mode: "require", verb: submit, cause: provokeUnknown, class: classFailed}, false},
		{"client failed", x12LoadReq{mode: "require", verb: cliNudge, class: classFailed, hasOp: true, op: closed(opjournal.ArmFailed, "boom")}, true},
		{"client failed, off", x12LoadReq{mode: "off", verb: cliNudge, class: classFailed}, true},
		{"no answer", x12LoadReq{mode: "auto", verb: submit, class: "no-answer"}, true},
		{"op failed under a success", x12LoadReq{mode: "auto", verb: submit, class: classDelivered, hasOp: true, op: closed(opjournal.ArmFailed, opjournal.ReasonOther)}, true},
	} {
		item, bad := x12NotSucceeded(&tc.r)
		if bad != tc.bad {
			t.Errorf("%s: not succeeded %v (%q), want %v", tc.name, bad, item, tc.bad)
		}
	}
	r := x12LoadReq{
		mode: "require", round: 2, verb: cliNudge, key: "x12-load-2-077", class: classFailed, hasOp: true, op: closed(opjournal.ArmFailed, "cli: boom"),
		orig: x12Answer{answered: true, status: 1, stderr: "gc nudge: boom"},
	}
	item, _ := x12NotSucceeded(&r)
	for _, want := range []string{"require round 3 cli-nudge-wait", `key "x12-load-2-077"`, "client failed", "gc nudge: boom", "op gcg-op1 failed{other}", `reason "cli: boom"`} {
		if !strings.Contains(item, want) {
			t.Errorf("the item %q lacks %q", item, want)
		}
	}
}

// TestX12LoadReportStatesLatencyPerBackendAndGrowth pins the load section
// of the report: its R2 table names the backend of every latency row, its
// R3 line names the arm whose op-row growth it states and prints a figure
// per op only when one was computed, it lists every well-formed request that
// did not succeed, and it states the load's wall time against its budget,
// or, for a load that stopped before its last round, how far it got.
func TestX12LoadReportStatesLatencyPerBackendAndGrowth(t *testing.T) {
	perOp := 150.0
	stated := x12Growth{Recorded: 5, NewRows: 5, LogicalByKind: map[string]x12Size{}, PhysicalBytes: 1000, PhysicalOverOff: 750, PhysicalPerOp: &perOp, PhysicalNote: "150 physical bytes per op over the off round"}
	unstated := x12Growth{Recorded: 5, NewRows: 5, LogicalByKind: map[string]x12Size{}, PhysicalBytes: 1000, PhysicalOverOff: -750, PhysicalNote: "no physical figure per op: round 5 (auto) shrank by 9 bytes, so the store compacted mid-load"}
	doc := x12ReportDoc{Tier: "evidence", Backend: "bd-dolt", Loads: []x12LoadRecord{{
		Tier: "evidence", Backend: "bd-dolt",
		Budget: 45 * time.Minute, Elapsed: 12 * time.Minute, Finished: true,
		WellFormed: 7, NotSucceeded: []string{"require round 3 cli-nudge-wait key \"k\": client failed"},
		R2: []x12Latency{{Backend: "bd-dolt", Mode: "auto", Path: "submit", N: 3, P50: time.Millisecond, P95: 2 * time.Millisecond, P99: 3 * time.Millisecond, Max: 4 * time.Millisecond}},
		R3: stated,
	}, {Tier: "evidence", Backend: "sqlite", Budget: 45 * time.Minute, Elapsed: 20 * time.Minute, R3: unstated}}}
	md := renderX12Markdown(doc)
	for _, want := range []string{
		"### Fault-free load (bd-dolt)",
		"Load wall time 12m0s of its 45m0s budget",
		"### Fault-free load (sqlite)\n\nLoad stopped before its last round after 20m0s\n",
		"Well-formed requests that did not succeed: 1 of 7\n- require round 3 cli-nudge-wait key \"k\": client failed\n",
		"| Backend | Mode | Path | n | p50 | p95 | p99 | max |",
		"| bd-dolt | auto | submit | 3 | 1ms | 2ms | 3ms | 4ms |",
		"R3 op-row growth (bd-dolt): 5 recorded requests, the store gained 5 op rows; physical 1000 bytes over the recorded rounds, 750 over their off rounds; 150 physical bytes per op over the off round;",
		"R3 op-row growth (sqlite): 5 recorded requests, the store gained 5 op rows; physical 1000 bytes over the recorded rounds, -750 over their off rounds; no physical figure per op: round 5 (auto) shrank",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("the report lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "(0 per op)") {
		t.Errorf("the report states a figure per op it did not compute:\n%s", md)
	}
	data, err := json.Marshal(unstated)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "physical_bytes_per_op") {
		t.Errorf("the JSON states a figure per op it did not compute: %s", data)
	}
	if data, _ = json.Marshal(stated); !strings.Contains(string(data), `"physical_bytes_per_op":150`) {
		t.Errorf("the JSON lacks the computed figure per op: %s", data)
	}
}

// TestX12PassLineBudget pins the budget line, judged from each load
// record's Budget, Elapsed and Finished, not from the problems its test
// wrote: not run without an evidence load, pass within the budget, FAIL past
// it; a load that stopped before its last round has not shown it finishes
// within its budget, so it leaves the line not run until it ran past it; an
// evidence load that carries no budget is a harness item.
func TestX12PassLineBudget(t *testing.T) {
	if got := x12PassLineOf(t, &x12Collector{evals: map[string]int{}}, x12LineBudget).Status; got != "not run" {
		t.Errorf("budget status with no load %q, want not run", got)
	}
	gating := &x12Collector{evals: map[string]int{}}
	gating.addLoad(x12LoadRecord{Tier: "gating", Backend: "sqlite", Budget: x12LoadBudgetFor("gating"), Elapsed: time.Minute, Finished: true})
	if got := x12PassLineOf(t, gating, x12LineBudget).Status; got != "not run" {
		t.Errorf("budget status with only a gating load %q, want not run", got)
	}
	if got := x12PassLineOf(t, gating, "harness").Status; got == "FAIL" {
		t.Errorf("a gating load without a budget is a harness item: %+v", x12PassLineOf(t, gating, "harness"))
	}
	within := &x12Collector{evals: map[string]int{}}
	within.addLoad(x12LoadRecord{Tier: "evidence", Backend: "bd-dolt", Budget: 90 * time.Minute, Elapsed: 63 * time.Minute, Finished: true})
	if got := x12PassLineOf(t, within, x12LineBudget); got.Status != "pass" || got.Measured != "bd-dolt: 1h3m0s of 1h30m0s" {
		t.Errorf("budget line within the budget = %+v, want pass measuring bd-dolt: 1h3m0s of 1h30m0s", got)
	}
	over := &x12Collector{evals: map[string]int{}}
	over.addLoad(x12LoadRecord{Tier: "evidence", Backend: "bd-dolt", Budget: 90 * time.Minute, Elapsed: 2 * time.Hour, Finished: true})
	got := x12PassLineOf(t, over, x12LineBudget)
	if got.Status != "FAIL" || len(got.Offending) != 1 || !strings.Contains(got.Offending[0], x12BudgetProblem(90*time.Minute, 2*time.Hour)) {
		t.Errorf("budget line past the budget, with no problem written = %+v, want FAIL naming the overrun", got)
	}
	// A problem the test wrote under the line is not counted a second time.
	written := &x12Collector{evals: map[string]int{}}
	written.addLoad(x12LoadRecord{Tier: "evidence", Backend: "bd-dolt", Budget: time.Minute, Elapsed: time.Hour, Finished: true, Problem: []string{"[" + x12LineBudget + "] " + x12BudgetProblem(time.Minute, time.Hour)}})
	if got := x12PassLineOf(t, written, x12LineBudget); got.Status != "FAIL" || len(got.Offending) != 1 {
		t.Errorf("budget line past the budget with its problem written = %+v, want FAIL with one item", got)
	}
	stopped := &x12Collector{evals: map[string]int{}}
	stopped.addLoad(x12LoadRecord{Tier: "evidence", Backend: "bd-dolt", Budget: 90 * time.Minute, Elapsed: 20 * time.Minute})
	if got := x12PassLineOf(t, stopped, x12LineBudget); got.Status != "not run" || got.Measured != "bd-dolt: stopped before its last round after 20m0s of 1h30m0s" {
		t.Errorf("budget line of a load that stopped within its budget = %+v, want not run, naming the stop", got)
	}
	stoppedOver := &x12Collector{evals: map[string]int{}}
	stoppedOver.addLoad(x12LoadRecord{Tier: "evidence", Backend: "bd-dolt", Budget: 90 * time.Minute, Elapsed: 2 * time.Hour})
	if got := x12PassLineOf(t, stoppedOver, x12LineBudget); got.Status != "FAIL" || len(got.Offending) != 1 {
		t.Errorf("budget line of a load that stopped past its budget = %+v, want FAIL with one item", got)
	}
	unbudgeted := &x12Collector{evals: map[string]int{}}
	unbudgeted.addLoad(x12LoadRecord{Tier: "evidence", Backend: "bd-dolt", Elapsed: 2 * time.Hour, Finished: true})
	if got := x12PassLineOf(t, unbudgeted, "harness"); got.Status != "FAIL" || len(got.Offending) != 1 || !strings.Contains(got.Offending[0], "no wall-time budget") {
		t.Errorf("harness line with an evidence load that carries no budget = %+v, want FAIL naming it", got)
	}
}

// TestX12LoadBudgetFor pins each tier's load budget: x12LoadBudget for the
// evidence load, none for the gating tier's small one.
func TestX12LoadBudgetFor(t *testing.T) {
	if got := x12LoadBudgetFor("evidence"); got != x12LoadBudget || got <= 0 {
		t.Errorf("evidence load budget %s, want %s", got, x12LoadBudget)
	}
	if got := x12LoadBudgetFor("gating"); got != 0 {
		t.Errorf("gating load budget %s, want none", got)
	}
}

// TestX12PassLineSucceeds pins the succeeds line, judged from each load
// record's WellFormed and NotSucceeded, not from the problems its test
// wrote: not run without a load, pass when every well-formed request
// succeeded, FAIL naming each that did not.
func TestX12PassLineSucceeds(t *testing.T) {
	if got := x12PassLineOf(t, &x12Collector{evals: map[string]int{}}, x12LineSucceeds).Status; got != "not run" {
		t.Errorf("succeeds status with no load %q, want not run", got)
	}
	ok := &x12Collector{evals: map[string]int{}}
	ok.addLoad(x12LoadRecord{Backend: "bd-dolt", WellFormed: 1000})
	if got := x12PassLineOf(t, ok, x12LineSucceeds); got.Status != "pass" || got.Measured != "0 offending of 1000 evaluated" {
		t.Errorf("succeeds line with every request succeeding = %+v, want pass, 0 offending of 1000", got)
	}
	bad := &x12Collector{evals: map[string]int{}}
	bad.addLoad(x12LoadRecord{Backend: "bd-dolt", WellFormed: 1000, NotSucceeded: []string{"require round 3 submit key \"k\": client failed"}})
	got := x12PassLineOf(t, bad, x12LineSucceeds)
	if got.Status != "FAIL" || len(got.Offending) != 1 || !strings.Contains(got.Offending[0], "require round 3 submit") {
		t.Errorf("succeeds line with a failed request and no problem written = %+v, want FAIL naming it", got)
	}
	written := &x12Collector{evals: map[string]int{}}
	written.addLoad(x12LoadRecord{Backend: "bd-dolt", WellFormed: 1000, NotSucceeded: []string{"x"}, Problem: []string{"[" + x12LineSucceeds + "] x"}})
	if got := x12PassLineOf(t, written, x12LineSucceeds); got.Status != "FAIL" || len(got.Offending) != 1 {
		t.Errorf("succeeds line with its problem also written = %+v, want FAIL with one item", got)
	}
}

// x12PassLineOf is r's pass line named line.
func x12PassLineOf(t *testing.T, r *x12Collector, line string) x12PassLine {
	t.Helper()
	for _, l := range r.passLines() {
		if l.Line == line {
			return l
		}
	}
	t.Fatalf("no %s pass line", line)
	return x12PassLine{}
}
