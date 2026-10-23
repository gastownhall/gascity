//go:build integration

package conversationlane_test

// The X12 report: the pass lines P1 to P5, I4, I8, R1, R3 and harness with
// their measured values and offending items, and the measurements R1 to R3,
// as JSON and as the Markdown table the PR body carries. Every pass line is
// asserted in the test that measures it; the report only aggregates, says
// "not run" for a line nothing evaluated, and is stamped FAILED when the
// test binary failed.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
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
	Problem []string                  `json:"problems,omitempty"`
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
	Rounds          []x12RoundGrowth `json:"rounds"`
	PhysicalBytes   int64            `json:"physical_bytes_recorded_rounds"`
	PhysicalOverOff int64            `json:"physical_bytes_over_off"`
	PhysicalPerOp   float64          `json:"physical_bytes_per_op"`
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
			offending[line] = append(offending[line], l.Backend+": "+p)
		}
	}
	for _, a := range r.arms {
		for _, p := range a.Problems {
			offending["I8"] = append(offending["I8"], a.Backend+"/"+a.Mode+": "+p)
		}
	}
	verdict := func(line string) string {
		switch {
		case len(offending[line]) > 0:
			return "FAIL"
		case r.evals[line] == 0 && (line != "P5" || r.evals[x12P5Identifiers] == 0):
			return "not run"
		}
		return "pass"
	}
	measured := func(line string) string {
		return fmt.Sprintf("%d offending of %d evaluated", len(offending[line]), r.evals[line])
	}
	p4 := measured("P4")
	for _, l := range r.loads {
		p4 = fmt.Sprintf("%s: %d/%d equal (%.4f), %d late, %d other, %d open unexplained", l.Backend, l.P4.Equal, l.P4.Requests, l.P4.EqualShare, len(l.P4.Late), len(l.P4.Other), len(l.P4.OpenUnexplained))
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
		fmt.Fprintf(&b, "P4: %d requests, %d equal (%.4f); late mismatches %d; other %d; open ops not replayed %d (%d unexplained); mail notifies excluded %d\n\n", l.P4.Requests, l.P4.Equal, l.P4.EqualShare, len(l.P4.Late), len(l.P4.Other), l.P4.NotReplay, len(l.P4.OpenUnexplained), l.P4.MailNotifyExcluded)
		for _, d := range l.P4.CodeDiffs {
			fmt.Fprintf(&b, "- code difference: %s\n", d)
		}
		b.WriteString("\nR1 refusals and unrecorded requests:\n\n| Mode/surface | Class | Count |\n|---|---|---|\n")
		for _, k := range sortedKeys(l.R1) {
			for _, c := range sortedKeys(l.R1[k]) {
				fmt.Fprintf(&b, "| %s | %s | %d |\n", k, c, l.R1[k][c])
			}
		}
		b.WriteString("\nR2 latency (reported, not asserted):\n\n| Mode | Path | n | p50 | p95 | p99 | max |\n|---|---|---|---|---|---|---|\n")
		for _, r := range l.R2 {
			fmt.Fprintf(&b, "| %s | %s | %d | %s | %s | %s | %s |\n", r.Mode, r.Path, r.N, r.P50, r.P95, r.P99, r.Max)
		}
		g := l.R3
		fmt.Fprintf(&b, "\nR3 op-row growth: %d recorded requests, the store gained %d op rows; physical %d bytes over the recorded rounds, %d over their off rounds (%.0f per op); %s\n\n", g.Recorded, g.NewRows, g.PhysicalBytes, g.PhysicalOverOff, g.PhysicalPerOp, g.DailyFormulaMD)
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
