package splittest

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Verdicts a baseline entry may carry. Every known site is adjudicated, so a
// reviewer reading the baseline sees what each one is, not only that it exists.
const (
	// VerdictMisroute is a production read or write that reaches the wrong
	// store on a split city: a routing defect with a fix owed (S3).
	VerdictMisroute = "misroute"
	// VerdictKnownViolation is a placement the owner has ruled against but
	// current code still relies on (rig-store graph beads, S4): tracked, not
	// allowed.
	VerdictKnownViolation = "known-violation"
	// VerdictRelic is a read that only finds the row because it was written
	// before the split (a pre-cutover copy in a scope store).
	VerdictRelic = "relic"
	// VerdictFixture is a test-fixture artifact: the site is only reached
	// because a fixture shaped the store in a way production never does.
	VerdictFixture = "fixture"
	// VerdictTriage is a site no one has adjudicated yet. The ratchet FAILS on
	// it: a baseline row is a ruling, not a placeholder.
	VerdictTriage = "triage"
)

var knownVerdicts = map[string]bool{
	VerdictMisroute:       true,
	VerdictKnownViolation: true,
	VerdictRelic:          true,
	VerdictFixture:        true,
	VerdictTriage:         true,
}

// baselineColumns is the baseline's tab-separated layout.
const baselineColumns = "file\tfunc\top\tview\tclass\tcaller\tmax_lines\tmax_caller_lines\ttests\tscope\tverdict\tnote"

// AnyTest is the scope of a row any test may reach: a production defect
// (misroute, known violation, relic) is the same row whoever trips it.
const AnyTest = "*"

// BaselineEntry is one known violation key in a package's checked-in
// baseline.
type BaselineEntry struct {
	Key SiteKey
	// MaxLines caps the distinct lines in Key.File's Key.Func the gate may
	// see violations at: a second offending line in a known function is a new
	// site, even under the same key.
	MaxLines int
	// MaxCallerLines caps the distinct lines of Key.Caller the key may be
	// reached from: a second call site in a known caller is new.
	MaxCallerLines int
	// Tests is the number of distinct tests that reached the key in the run
	// the baseline was taken from.
	Tests int
	// Scope is AnyTest, or the tests the row is bound to: a fixture row
	// excuses a test-only shape for exactly the tests that have it, so the
	// same key reached from any other test is new.
	Scope []string
	// Verdict is one of the Verdict* constants.
	Verdict string
	// Note says why, and which fix owns it.
	Note string
}

// anyTest reports a row any test may reach.
func (e BaselineEntry) anyTest() bool { return len(e.Scope) == 1 && e.Scope[0] == AnyTest }

// ParseBaseline reads a baseline: tab-separated rows in baselineColumns order;
// blank lines and lines starting with '#' are skipped. A fixture row must be
// bound to its tests; every other verdict names a production defect and is
// scoped to any test.
func ParseBaseline(r io.Reader) ([]BaselineEntry, error) {
	var entries []BaselineEntry
	seen := map[SiteKey]int{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 12 {
			return nil, fmt.Errorf("line %d: want 12 tab-separated fields (%s), got %d", lineNo, strings.ReplaceAll(baselineColumns, "\t", ", "), len(f))
		}
		for i := 0; i < 6; i++ {
			if strings.TrimSpace(f[i]) == "" {
				return nil, fmt.Errorf("line %d: empty key field %d", lineNo, i+1)
			}
		}
		maxLines, err := strconv.Atoi(f[6])
		if err != nil || maxLines < 1 {
			return nil, fmt.Errorf("line %d: max_lines %q is not a positive integer", lineNo, f[6])
		}
		maxCallerLines, err := strconv.Atoi(f[7])
		if err != nil || maxCallerLines < 0 {
			return nil, fmt.Errorf("line %d: max_caller_lines %q is not a non-negative integer", lineNo, f[7])
		}
		tests, err := strconv.Atoi(f[8])
		if err != nil || tests < 0 {
			return nil, fmt.Errorf("line %d: tests %q is not a non-negative integer", lineNo, f[8])
		}
		scope := strings.Split(strings.TrimSpace(f[9]), ",")
		if strings.TrimSpace(f[9]) == "" {
			return nil, fmt.Errorf("line %d: empty scope; give %q or the tests the row is bound to", lineNo, AnyTest)
		}
		verdict := strings.TrimSpace(f[10])
		if !knownVerdicts[verdict] {
			return nil, fmt.Errorf("line %d: verdict %q is not one of misroute, known-violation, relic, fixture, triage", lineNo, verdict)
		}
		entry := BaselineEntry{
			Key:      SiteKey{File: f[0], Func: f[1], Op: f[2], View: f[3], Class: f[4], Caller: f[5]},
			MaxLines: maxLines, MaxCallerLines: maxCallerLines, Tests: tests, Scope: scope,
			Verdict: verdict, Note: strings.TrimSpace(f[11]),
		}
		if verdict == VerdictFixture && entry.anyTest() {
			return nil, fmt.Errorf("line %d: a fixture row must be bound to the tests that reach it, not %q", lineNo, AnyTest)
		}
		if entry.Note == "" {
			return nil, fmt.Errorf("line %d: empty note; every known site says why it is there", lineNo)
		}
		if prev, dup := seen[entry.Key]; dup {
			return nil, fmt.Errorf("line %d: key %s repeats line %d", lineNo, entry.Key, prev)
		}
		seen[entry.Key] = lineNo
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// LineCapExcess is a known function the run saw offending, or calling, at
// more distinct lines than the baseline allows.
type LineCapExcess struct {
	File, Func string
	Lines      []int
	Cap        int
	// Caller marks a cap on the caller's lines rather than the site's.
	Caller bool
}

// OutOfScope is a known fixture key reached from tests its row is not bound
// to.
type OutOfScope struct {
	Site  SiteSummary
	Tests []string
}

// RatchetResult compares one run's observed sites with the baseline.
type RatchetResult struct {
	// Observed is every key the run reached.
	Observed []SiteSummary
	// New are observed keys the baseline does not list.
	New []SiteSummary
	// OutOfScope are fixture keys reached from tests their rows do not name.
	OutOfScope []OutOfScope
	// Triage are observed keys the baseline lists as unadjudicated.
	Triage []SiteSummary
	// LineCap are known functions seen at more lines than the baseline caps.
	LineCap []LineCapExcess
	// Grown are known keys more tests hit than the baseline counted
	// (informational: counts are per full run, a shard sees fewer).
	Grown []SiteSummary
	// BaselineSize is the number of baseline entries.
	BaselineSize int
}

// Failed reports whether the ratchet fails the run.
func (r RatchetResult) Failed() bool {
	return len(r.New) > 0 || len(r.OutOfScope) > 0 || len(r.Triage) > 0 || len(r.LineCap) > 0
}

// CompareToBaseline runs the ratchet.
func CompareToBaseline(observed []SiteSummary, baseline []BaselineEntry) RatchetResult {
	known := make(map[SiteKey]BaselineEntry, len(baseline))
	siteCaps := map[[2]string]int{}
	callerCaps := map[string]int{}
	for _, entry := range baseline {
		known[entry.Key] = entry
		fn := [2]string{entry.Key.File, entry.Key.Func}
		siteCaps[fn] = max(siteCaps[fn], entry.MaxLines)
		callerCaps[entry.Key.Caller] = max(callerCaps[entry.Key.Caller], entry.MaxCallerLines)
	}
	result := RatchetResult{Observed: observed, BaselineSize: len(baseline)}
	siteLines := map[[2]string]map[int]bool{}
	callerLines := map[string]map[int]bool{}
	for _, site := range observed {
		entry, ok := known[site.Key]
		if !ok {
			result.New = append(result.New, site)
			continue
		}
		if entry.Verdict == VerdictTriage {
			result.Triage = append(result.Triage, site)
		}
		if !entry.anyTest() {
			bound := map[string]bool{}
			for _, t := range entry.Scope {
				bound[t] = true
			}
			var stray []string
			for _, t := range site.TestNames {
				if !bound[t] {
					stray = append(stray, t)
				}
			}
			if len(stray) > 0 {
				result.OutOfScope = append(result.OutOfScope, OutOfScope{Site: site, Tests: stray})
			}
		}
		if site.Tests > entry.Tests {
			result.Grown = append(result.Grown, site)
		}
		fn := [2]string{site.Key.File, site.Key.Func}
		if siteLines[fn] == nil {
			siteLines[fn] = map[int]bool{}
		}
		for _, l := range site.Lines {
			siteLines[fn][l] = true
		}
		if site.Key.Caller != "-" {
			if callerLines[site.Key.Caller] == nil {
				callerLines[site.Key.Caller] = map[int]bool{}
			}
			for _, l := range site.CallerLines {
				callerLines[site.Key.Caller][l] = true
			}
		}
	}
	for fn, lines := range siteLines {
		if len(lines) > siteCaps[fn] {
			result.LineCap = append(result.LineCap, LineCapExcess{File: fn[0], Func: fn[1], Lines: sortedInts(lines), Cap: siteCaps[fn]})
		}
	}
	for caller, lines := range callerLines {
		if len(lines) > callerCaps[caller] {
			result.LineCap = append(result.LineCap, LineCapExcess{Func: caller, Lines: sortedInts(lines), Cap: callerCaps[caller], Caller: true})
		}
	}
	sort.Slice(result.LineCap, func(i, j int) bool {
		return result.LineCap[i].File+result.LineCap[i].Func < result.LineCap[j].File+result.LineCap[j].Func
	})
	return result
}

// Report renders the result for the test log.
func (r RatchetResult) Report(baselinePath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "split-strict: %d violation key(s) observed; the baseline %s lists %d.\n", len(r.Observed), baselinePath, r.BaselineSize)
	for _, site := range r.Grown {
		fmt.Fprintf(&b, "split-strict: known key reached by more tests than the baseline counts: %s (%d tests)\n", site.Key, site.Tests)
	}
	if !r.Failed() {
		return b.String()
	}
	for _, site := range r.New {
		fmt.Fprintf(&b, "  NEW %s:%s %s: %s of %s rows through the %s view, from %s; %d test(s), e.g. %s\n",
			site.Key.File, joinInts(site.Lines), site.Key.Func, site.Key.Op, site.Key.Class, site.Key.View,
			site.Key.Caller, site.Tests, site.SampleTest)
	}
	for _, o := range r.OutOfScope {
		fmt.Fprintf(&b, "  NEW %s: a fixture row reached from test(s) it is not bound to: %s\n", o.Site.Key, strings.Join(o.Tests, ","))
	}
	for _, site := range r.Triage {
		fmt.Fprintf(&b, "  TRIAGE %s: the baseline lists it unadjudicated; give it a verdict\n", site.Key)
	}
	for _, x := range r.LineCap {
		if x.Caller {
			fmt.Fprintf(&b, "  LINES caller %s: reaches known keys from lines %s, the baseline caps it at %d\n", x.Func, joinInts(x.Lines), x.Cap)
			continue
		}
		fmt.Fprintf(&b, "  LINES %s %s: offending at lines %s, the baseline caps it at %d\n", x.File, x.Func, joinInts(x.Lines), x.Cap)
	}
	b.WriteString("Each NEW key reads or writes a row of one class through the store of another: on a split city that read comes back empty. " +
		"Route it to the store that owns the class; if it is a deliberate federation leg or residence probe, wrap the READ in beads.FederatedRead " +
		"(a reviewed marker); rerun one test with GC_TEST_SPLIT_STRICT_PANIC=1 for the full stack.\n")
	return b.String()
}

func joinInts(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

// observedColumns is the observed site table's layout.
const observedColumns = "file\tfunc\top\tview\tclass\tcaller\tlines\tcaller_lines\ttests\tevents\ttest_names\tsample_via"

// FormatSiteTable renders observed keys as the tab-separated table the gate
// writes per run (observedColumns).
func FormatSiteTable(sites []SiteSummary) string {
	var b strings.Builder
	b.WriteString("# " + observedColumns + "\n")
	for _, s := range sites {
		k := s.Key
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\n", k.File, k.Func, k.Op, k.View, k.Class, k.Caller,
			joinInts(s.Lines), joinInts(s.CallerLines), s.Tests, s.Events, strings.Join(s.TestNames, ","), s.SampleVia)
	}
	return b.String()
}
