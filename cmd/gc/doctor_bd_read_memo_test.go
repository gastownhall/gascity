package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// fixtureBd is a fake bd executable for one scope: it answers the list, query
// and ready reads a BdStore issues from a fixed set of rows, applies
// `update --set-metadata` writes, and counts every invocation. It filters on
// the flags the doctor's store reads use, which is enough to drive the real
// checks through a real BdStore.
type fixtureBd struct {
	mu    sync.Mutex
	rows  []fixtureBdRow
	calls atomic.Int64
	// gate, when set, blocks every read until it is closed.
	gate chan struct{}
}

type fixtureBdRow struct {
	ID        string            `json:"id"`
	Title     string            `json:"title"`
	Status    string            `json:"status"`
	IssueType string            `json:"issue_type"`
	Assignee  string            `json:"assignee"`
	Labels    []string          `json:"labels"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Ephemeral bool              `json:"ephemeral,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

func (f *fixtureBd) run(_ string, name string, args ...string) ([]byte, error) {
	f.calls.Add(1)
	if name != "bd" || len(args) == 0 {
		return nil, fmt.Errorf("fixture bd: unexpected %s %v", name, args)
	}
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch args[0] {
	case "list":
		return f.match(false, args[1:], nil)
	case "query":
		if len(args) < 3 {
			return nil, fmt.Errorf("fixture bd: query without expression")
		}
		return f.match(true, args[3:], strings.Split(args[2], " AND "))
	case "ready":
		return []byte("[]"), nil
	case "update":
		return nil, f.update(args[1:])
	}
	return nil, fmt.Errorf("fixture bd: unsupported %v", args)
}

func (f *fixtureBd) update(args []string) error {
	var id string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--set-metadata" && i+1 < len(args):
			k, v, _ := strings.Cut(args[i+1], "=")
			for j := range f.rows {
				if f.rows[j].ID == id {
					f.rows[j].Metadata[k] = v
					return nil
				}
			}
			return fmt.Errorf("fixture bd: no issue %q", id)
		case !strings.HasPrefix(args[i], "-"):
			id = args[i]
		}
	}
	return fmt.Errorf("fixture bd: unsupported update %v", args)
}

func (f *fixtureBd) match(ephemeral bool, flags, clauses []string) ([]byte, error) {
	var label, status, typ string
	meta := map[string]string{}
	all := false
	for i := 0; i < len(flags); i++ {
		flag := flags[i]
		switch {
		case strings.HasPrefix(flag, "--label="):
			label = strings.TrimPrefix(flag, "--label=")
		case strings.HasPrefix(flag, "--status="):
			status = strings.TrimPrefix(flag, "--status=")
		case strings.HasPrefix(flag, "--type="):
			typ = strings.TrimPrefix(flag, "--type=")
		case flag == "--metadata-field" && i+1 < len(flags):
			k, v, _ := strings.Cut(flags[i+1], "=")
			meta[k] = v
			i++
		case flag == "--all":
			all = true
		}
	}
	for _, clause := range clauses {
		k, v, _ := strings.Cut(strings.TrimSpace(clause), "=")
		switch k {
		case "label":
			label = v
		case "status":
			status = v
		case "type":
			typ = v
		}
	}
	out := []fixtureBdRow{}
	for _, row := range f.rows {
		switch {
		case row.Ephemeral != ephemeral,
			label != "" && !slices.Contains(row.Labels, label),
			status != "" && row.Status != status,
			status == "" && !all && row.Status == "closed",
			typ != "" && row.IssueType != typ:
			continue
		}
		matched := true
		for k, v := range meta {
			if row.Metadata[k] != v {
				matched = false
			}
		}
		if matched {
			out = append(out, row)
		}
	}
	return json.Marshal(out)
}

// fixtureCity is a city plus one rig, each scope served by its own fixture bd,
// shaped like the cities doctor sees in the field: bound PackV2 agents, a pool
// template, session beads and routed work.
type fixtureCity struct {
	cityPath string
	rigPath  string
	cfg      *config.City
	bds      map[string]*fixtureBd
}

func newFixtureCity(t *testing.T, broken bool) *fixtureCity {
	t.Helper()
	cityPath := t.TempDir()
	rigPath := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "dog", BindingName: "gastown"},
			{Name: "polecat", Dir: "repo", BindingName: "gastown"},
			{Name: "builder", Dir: "repo"},
			{Name: "reviewer", Dir: "repo"},
		},
		Rigs: []config.Rig{{Name: "repo", Path: rigPath}},
	}
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	session := func(id, template, state string) fixtureBdRow {
		return fixtureBdRow{
			ID: id, Title: template, Status: "open", IssueType: "session", Labels: []string{"gc:session"},
			Metadata:  map[string]string{"template": template, "state": state, "session_name": id},
			CreatedAt: created,
		}
	}
	task := func(id, title string, labels []string, meta map[string]string) fixtureBdRow {
		if meta == nil {
			meta = map[string]string{}
		}
		return fixtureBdRow{ID: id, Title: title, Status: "open", IssueType: "task", Labels: labels, Metadata: meta, CreatedAt: created}
	}
	city := &fixtureBd{rows: []fixtureBdRow{
		task("CITY-1", "warrant", nil, map[string]string{"gc.routed_to": "gastown.dog"}),
		session("CITY-S1", "repo/builder", "asleep"),
	}}
	rig := &fixtureBd{rows: []fixtureBdRow{
		task("RIG-1", "work", nil, map[string]string{"gc.routed_to": "repo/gastown.polecat"}),
		session("RIG-S1", "repo/builder", "asleep"),
		session("RIG-S2", "repo/reviewer", "asleep"),
	}}
	if broken {
		// Short-form routes to bound agents, retired hold labels, a wisp the
		// tier-merged reads see, and an idle pool instance next to unclaimed
		// routed work.
		city.rows = append(city.rows,
			task("CITY-2", "short warrant", nil, map[string]string{"gc.routed_to": "dog"}),
			task("CITY-3", "parked", []string{"on-hold", "human"}, nil),
		)
		wisp := task("RIG-W1", "stalled wisp", []string{"blocked-on-upstream"}, nil)
		wisp.Ephemeral = true
		rig.rows = append(rig.rows,
			task("RIG-2", "short work", nil, map[string]string{"gc.routed_to": "repo/polecat"}),
			task("RIG-3", "waiting", []string{"blocked"}, nil),
			wisp,
			session("RIG-S3", "repo/builder", "active"),
			task("RIG-4", "pool work", nil, map[string]string{"gc.routed_to": "repo/builder"}),
		)
	}
	return &fixtureCity{
		cityPath: cityPath,
		rigPath:  rigPath,
		cfg:      cfg,
		bds:      map[string]*fixtureBd{cityPath: city, rigPath: rig},
	}
}

// storeFactory opens a BdStore per scope over that scope's fixture bd and
// shares it for the run exactly as gc doctor's perRunStoreFactory does. Like
// the bd store constructors, it passes each runner through withBdReadMemo, so
// a store reads through the memo while a doctor run has one installed for
// this city.
func (fc *fixtureCity) storeFactory() func(string) (beads.Store, error) {
	return perRunStoreFactory(func(dir string) (beads.Store, error) {
		bd, ok := fc.bds[dir]
		if !ok {
			return nil, fmt.Errorf("no fixture store at %q", dir)
		}
		return beads.NewBdStore(dir, withBdReadMemo(fc.cityPath, bd.run)), nil
	})
}

func (fc *fixtureCity) calls() int64 {
	var n int64
	for _, bd := range fc.bds {
		n += bd.calls.Load()
	}
	return n
}

// doctorChecks returns the store-backed checks that issue the most bd reads
// on a live city, in doctor's registration order.
func (fc *fixtureCity) doctorChecks(factory func(string) (beads.Store, error)) []doctor.Check {
	return []doctor.Check{
		newV2RoutedToNamespaceCheck(fc.cfg, fc.cityPath, factory),
		newExecutorIdentityResidueCheck(fc.cfg, fc.cityPath, factory),
		newPoolIdleRoutedWorkCheck(fc.cfg, fc.cityPath, factory),
		newHoldLabelConventionsCheck(fc.cityPath, "city", factory),
		newHoldLabelConventionsCheck(fc.rigPath, "repo", factory),
	}
}

// runDoctor runs the checks that checks builds over this run's stores and
// returns the streamed text report and the --json report. With memoize set,
// the read memo is installed by installDoctorBdReadMemo, exactly as doDoctor
// installs it.
func (fc *fixtureCity) runDoctor(t *testing.T, memoize, fix bool, checks func(func(string) (beads.Store, error)) []doctor.Check) (string, string) {
	t.Helper()
	d := &doctor.Doctor{}
	if memoize {
		defer installDoctorBdReadMemo(d, fc.cityPath)()
	}
	for _, c := range checks(fc.storeFactory()) {
		d.Register(c)
	}
	var text bytes.Buffer
	report := d.Run(&doctor.CheckContext{CityPath: fc.cityPath, Verbose: true}, &text, fix)
	doctor.PrintSummary(&text, report)
	var js bytes.Buffer
	if err := writeDoctorJSON(&js, report); err != nil {
		t.Fatalf("writeDoctorJSON: %v", err)
	}
	return strings.ReplaceAll(strings.ReplaceAll(text.String(), fc.cityPath, "$CITY"), fc.rigPath, "$RIG"),
		strings.ReplaceAll(strings.ReplaceAll(js.String(), fc.cityPath, "$CITY"), fc.rigPath, "$RIG")
}

// TestDoctorBdReadMemoReportIsByteIdentical is the equivalence gate for the
// per-run bd read memo: over the same fixture city, doctor's text and JSON
// reports are byte-identical with and without it, in a healthy and a broken
// state, while the memo forks bd fewer times.
func TestDoctorBdReadMemoReportIsByteIdentical(t *testing.T) {
	for _, tc := range []struct {
		name     string
		broken   bool
		wantText string
	}{
		{
			name: "healthy",
			wantText: `  ✓ v2-routed-to-namespace — no short-form gc.routed_to values targeting bound agents found
  ✓ executor-identity-residue — no stale executor-identity stamp residue found
  ✓ pool-idle-routed-work — no pool has unclaimed routed work sitting beside an idle instance
  ✓ hold-label-conventions:city — no retired hold/blocked labels found in city
  ✓ hold-label-conventions:repo — no retired hold/blocked labels found in repo

5 passed
`,
		},
		{
			name:   "broken",
			broken: true,
			wantText: `  ⚠ v2-routed-to-namespace — 2 short-form gc.routed_to value(s) target bound PackV2 agents
      city bead CITY-2 has gc.routed_to="dog"; use "gastown.dog"
      rig repo bead RIG-2 has gc.routed_to="repo/polecat"; use "repo/gastown.polecat"
      hint: run gc doctor --fix to rewrite gc.routed_to to the binding-qualified agent name, then rerun gc doctor
  ✓ executor-identity-residue — no stale executor-identity stamp residue found
  ⚠ pool-idle-routed-work — 1 pool(s) have unclaimed routed work while an instance sits idle
      rig repo pool repo/builder has 1 unclaimed routed bead(s) (RIG-4) while 1 instance(s) sit idle (RIG-S3)
      hint: nudge the idle instance (gc session nudge <name>) or investigate why it has not claimed the routed work
  ✗ hold-label-conventions:city — 1 retired hold/blocked label use(s) found in city (advisory)
      retired label "on-hold" on CITY-3 "parked"
      hint: ` + holdLabelConventionsFixHint + `
  ✗ hold-label-conventions:repo — 1 retired hold/blocked label use(s) found in repo (advisory)
      retired label "blocked" on RIG-3 "waiting"
      hint: ` + holdLabelConventionsFixHint + `

1 passed, 2 warnings, 2 failed, 2 advisory
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plain := newFixtureCity(t, tc.broken)
			plainText, plainJSON := plain.runDoctor(t, false, false, plain.doctorChecks)

			memoized := newFixtureCity(t, tc.broken)
			memoText, memoJSON := memoized.runDoctor(t, true, false, memoized.doctorChecks)

			if plainText != tc.wantText {
				t.Errorf("doctor text report changed:\n--- got ---\n%s\n--- want ---\n%s", plainText, tc.wantText)
			}
			if memoText != plainText {
				t.Errorf("memoized text report differs:\n--- memo ---\n%s\n--- plain ---\n%s", memoText, plainText)
			}
			if memoJSON != plainJSON {
				t.Errorf("memoized JSON report differs:\n--- memo ---\n%s\n--- plain ---\n%s", memoJSON, plainJSON)
			}
			if got, base := memoized.calls(), plain.calls(); got >= base {
				t.Errorf("memoized run forked bd %d times, plain run %d; the memo saved nothing", got, base)
			} else {
				t.Logf("bd forks: %d plain, %d memoized", base, got)
			}
		})
	}
}

// TestDoctorBdReadMemoFixSeesItsOwnWrites runs --fix through the memo: the
// verifying re-run must read the rewritten routes, not the pre-fix answers,
// and the report must match a run without the memo.
func TestDoctorBdReadMemoFixSeesItsOwnWrites(t *testing.T) {
	plain := newFixtureCity(t, true)
	plainText, plainJSON := plain.runDoctor(t, false, true, plain.doctorChecks)

	memoized := newFixtureCity(t, true)
	memoText, memoJSON := memoized.runDoctor(t, true, true, memoized.doctorChecks)

	if !strings.Contains(plainText, "✓ v2-routed-to-namespace — no short-form gc.routed_to values targeting bound agents found (fixed)") {
		t.Fatalf("fixture --fix did not fix the routes:\n%s", plainText)
	}
	if memoText != plainText || memoJSON != plainJSON {
		t.Fatalf("memoized --fix report differs:\n--- memo ---\n%s\n--- plain ---\n%s", memoText, plainText)
	}
}

// labelStrippingFixCheck flags the beads that carry label and fixes them the
// way a pack script's remediation does: by rewriting the scope's ledger
// directly rather than through this process's stores, so no memoized runner
// ever sees the write.
type labelStrippingFixCheck struct {
	bd       *fixtureBd
	dir      string
	label    string
	newStore func(string) (beads.Store, error)
}

func (c *labelStrippingFixCheck) Name() string         { return "strip-label" }
func (c *labelStrippingFixCheck) CanFix() bool         { return true }
func (c *labelStrippingFixCheck) WarmupEligible() bool { return false }

func (c *labelStrippingFixCheck) Run(*doctor.CheckContext) *doctor.CheckResult {
	store, err := c.newStore(c.dir)
	if err != nil {
		return errorCheck(c.Name(), err.Error(), "", nil)
	}
	found, err := store.ListByLabel(c.label, 0)
	switch {
	case err != nil:
		return errorCheck(c.Name(), err.Error(), "", nil)
	case len(found) > 0:
		return errorCheck(c.Name(), fmt.Sprintf("%d bead(s) carry %q", len(found), c.label), "", nil)
	}
	return okCheck(c.Name(), fmt.Sprintf("no bead carries %q", c.label))
}

func (c *labelStrippingFixCheck) Fix(*doctor.CheckContext) error {
	c.bd.mu.Lock()
	defer c.bd.mu.Unlock()
	for i := range c.bd.rows {
		c.bd.rows[i].Labels = slices.DeleteFunc(c.bd.rows[i].Labels, func(l string) bool { return l == c.label })
	}
	return nil
}

// TestDoctorBdReadMemoRereadsAfterAFixThatBypassesIt runs --fix with a
// remediation that writes the ledger outside this process's stores. The memo
// cannot see that write, so only the BeforeFix hook installDoctorBdReadMemo
// wires keeps the verifying re-run, and a later check asking the same
// question, from being answered with the pre-fix read: the report must match
// a run without the memo.
func TestDoctorBdReadMemoRereadsAfterAFixThatBypassesIt(t *testing.T) {
	run := func(memoize bool) (string, string) {
		fc := newFixtureCity(t, true)
		return fc.runDoctor(t, memoize, true, func(factory func(string) (beads.Store, error)) []doctor.Check {
			return []doctor.Check{
				&labelStrippingFixCheck{bd: fc.bds[fc.cityPath], dir: fc.cityPath, label: "on-hold", newStore: factory},
				newHoldLabelConventionsCheck(fc.cityPath, "city", factory),
			}
		})
	}
	plainText, plainJSON := run(false)
	memoText, memoJSON := run(true)

	for _, want := range []string{
		`✓ strip-label — no bead carries "on-hold" (fixed)`,
		"✓ hold-label-conventions:city — no retired hold/blocked labels found in city",
	} {
		if !strings.Contains(plainText, want) {
			t.Fatalf("fixture --fix report lacks %q:\n%s", want, plainText)
		}
	}
	if memoText != plainText || memoJSON != plainJSON {
		t.Fatalf("memoized --fix report differs:\n--- memo ---\n%s\n--- plain ---\n%s", memoText, plainText)
	}
}

func TestBdReadMemoClassify(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want bdMemoClass
	}{
		{"bd", []string{"list", "--json"}, bdMemoCacheable},
		{"bd", []string{"query", "--json", "ephemeral=true"}, bdMemoCacheable},
		{"bd", []string{"ready", "--json"}, bdMemoCacheable},
		{"bd", []string{"show", "GA-1", "--json"}, bdMemoCacheable},
		{"bd", []string{"--readonly", "list", "--json"}, bdMemoCacheable},
		{"bd", []string{"version"}, bdMemoPassThrough},
		{"bd", []string{"config", "get", "types.custom"}, bdMemoPassThrough},
		{"bd", []string{"config", "set", "types.custom", "x"}, bdMemoInvalidating},
		{"bd", []string{"update", "--json", "GA-1"}, bdMemoInvalidating},
		{"bd", []string{"create", "--json"}, bdMemoInvalidating},
		{"bd", []string{"close", "GA-1"}, bdMemoInvalidating},
		{"bd", []string{"--readonly"}, bdMemoInvalidating},
		{"bd", nil, bdMemoInvalidating},
		{"dolt", []string{"sql", "-q", "select 1"}, bdMemoInvalidating},
	} {
		if got := bdReadMemoClassify(tc.name, tc.args); got != tc.want {
			t.Errorf("bdReadMemoClassify(%s %v) = %d, want %d", tc.name, tc.args, got, tc.want)
		}
	}
}

// countingRunner answers every call with its argv and counts it.
type countingRunner struct {
	calls atomic.Int64
	err   error
}

func (r *countingRunner) run(dir, name string, args ...string) ([]byte, error) {
	n := r.calls.Add(1)
	if r.err != nil {
		return nil, r.err
	}
	return []byte(fmt.Sprintf("%s|%s|%s|%d", dir, name, strings.Join(args, " "), n)), nil
}

func TestBdReadMemoServesRepeatedReadOnce(t *testing.T) {
	inner := &countingRunner{}
	run := newBdReadMemo("/city").wrap(inner.run)

	first, err := run("/city", "bd", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	first[0] = 'X' // a caller scribbling on its copy must not reach the memo
	second, err := run("/city", "bd", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("bd forked %d times for one repeated read, want 1", got)
	}
	if string(second) != "/city|bd|list --json|1" {
		t.Fatalf("second read = %q, want the first read's bytes", second)
	}
	if _, err := run("/rig", "bd", "list", "--json"); err != nil {
		t.Fatal(err)
	}
	if _, err := run("/city", "bd", "list", "--json", "--all"); err != nil {
		t.Fatal(err)
	}
	if got := inner.calls.Load(); got != 3 {
		t.Fatalf("bd forked %d times, want 3: a different dir or argv is a different read", got)
	}
}

func TestBdReadMemoNeverReplaysFailure(t *testing.T) {
	inner := &countingRunner{err: errors.New("connection refused")}
	run := newBdReadMemo("/city").wrap(inner.run)
	for i := 0; i < 2; i++ {
		if _, err := run("/city", "bd", "list", "--json"); err == nil {
			t.Fatal("expected the runner's error")
		}
	}
	if got := inner.calls.Load(); got != 2 {
		t.Fatalf("bd forked %d times, want 2: a failed read must be retried, not replayed", got)
	}
}

func TestBdReadMemoWriteAndInvalidateDropReads(t *testing.T) {
	inner := &countingRunner{}
	memo := newBdReadMemo("/city")
	run := memo.wrap(inner.run)
	read := func() {
		t.Helper()
		if _, err := run("/city", "bd", "list", "--json"); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if _, err := run("/city", "bd", "update", "--json", "GA-1", "--set-metadata", "k=v"); err != nil {
		t.Fatal(err)
	}
	read()
	if got := inner.calls.Load(); got != 3 {
		t.Fatalf("bd forked %d times, want 3: a write must drop the memoized read", got)
	}
	read()
	memo.invalidate()
	read()
	if got := inner.calls.Load(); got != 4 {
		t.Fatalf("bd forked %d times, want 4: invalidate must drop the memoized read", got)
	}
	// Pass-through probes leave the memo alone.
	if _, err := run("/city", "bd", "version"); err != nil {
		t.Fatal(err)
	}
	read()
	if got := inner.calls.Load(); got != 5 {
		t.Fatalf("bd forked %d times, want 5: a version probe must not drop memoized reads", got)
	}
}

func TestBdReadMemoIsPerRunner(t *testing.T) {
	inner := &countingRunner{}
	memo := newBdReadMemo("/city")
	a, b := memo.wrap(inner.run), memo.wrap(inner.run)
	for _, run := range []beads.CommandRunner{a, b, a, b} {
		if _, err := run("/city", "bd", "list", "--json"); err != nil {
			t.Fatal(err)
		}
	}
	if got := inner.calls.Load(); got != 2 {
		t.Fatalf("bd forked %d times, want 2: each runner (store) keeps its own reads", got)
	}
}

func TestBdReadMemoSharesInFlightRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bd := &fixtureBd{gate: make(chan struct{})}
		run := newBdReadMemo("/city").wrap(bd.run)
		const readers = 6
		var wg sync.WaitGroup
		outs := make([]string, readers)
		for i := 0; i < readers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				out, err := run("/city", "bd", "query", "--json", "ephemeral=true", "--limit", "0")
				if err != nil {
					t.Error(err)
				}
				outs[i] = string(out)
			}(i)
		}
		// Every reader is now either inside the one fork or waiting on it.
		synctest.Wait()
		close(bd.gate)
		wg.Wait()
		if got := bd.calls.Load(); got != 1 {
			t.Fatalf("bd forked %d times for %d concurrent identical reads, want 1", got, readers)
		}
		for _, out := range outs {
			if out != "[]" {
				t.Fatalf("reader got %q, want []", out)
			}
		}
	})
}

// TestBdReadMemoNeverSharesAFailedInFlightRead: readers that joined a read
// still in flight must each run their own read when it fails, never inherit
// its failure or an empty answer.
func TestBdReadMemoNeverSharesAFailedInFlightRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		var calls atomic.Int64
		run := newBdReadMemo("/city").wrap(func(string, string, ...string) ([]byte, error) {
			if calls.Add(1) == 1 {
				<-gate
				return nil, errors.New("connection refused")
			}
			return []byte("[]"), nil
		})
		read := func() ([]byte, error) { return run("/city", "bd", "list", "--json") }

		var wg sync.WaitGroup
		var leaderErr error
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, leaderErr = read()
		}()
		synctest.Wait() // the leader is inside the one fork
		const waiters = 5
		outs := make([][]byte, waiters)
		errs := make([]error, waiters)
		for i := range waiters {
			wg.Add(1)
			go func() {
				defer wg.Done()
				outs[i], errs[i] = read()
			}()
		}
		synctest.Wait()
		if got := calls.Load(); got != 1 {
			t.Fatalf("bd forked %d times while the first read was in flight, want 1: identical readers must join it", got)
		}
		close(gate)
		wg.Wait()

		if leaderErr == nil {
			t.Fatal("the first read succeeded, want the runner's error")
		}
		for i := range waiters {
			if errs[i] != nil || string(outs[i]) != "[]" {
				t.Fatalf("waiter %d got (%q, %v), want its own successful read", i, outs[i], errs[i])
			}
		}
		if got := calls.Load(); got != 1+waiters {
			t.Fatalf("bd forked %d times, want %d: each waiter re-runs a read that failed", got, 1+waiters)
		}
	})
}

func TestWithBdReadMemoOnlyWrapsTheDoctoredCity(t *testing.T) {
	inner := &countingRunner{}
	if run := withBdReadMemo("/city", inner.run); run == nil {
		t.Fatal("withBdReadMemo returned nil")
	}
	restore := installBdReadMemo(newBdReadMemo("/city"))
	defer restore()

	other := withBdReadMemo("/elsewhere", inner.run)
	for i := 0; i < 2; i++ {
		if _, err := other("/elsewhere", "bd", "list", "--json"); err != nil {
			t.Fatal(err)
		}
	}
	if got := inner.calls.Load(); got != 2 {
		t.Fatalf("bd forked %d times for another city's store, want 2 (no memo)", got)
	}
	doctored := withBdReadMemo("/city", inner.run)
	for i := 0; i < 2; i++ {
		if _, err := doctored("/city", "bd", "list", "--json"); err != nil {
			t.Fatal(err)
		}
	}
	if got := inner.calls.Load(); got != 3 {
		t.Fatalf("bd forked %d times, want 3: the doctored city's store reads once", got)
	}
	restore()
	if _, ok := activeBdReadMemos.Load(normalizePathForCompare("/city")); ok {
		t.Fatal("restore left the memo installed")
	}
}

// TestDoctorRoutedToForksDoNotScaleWithRoutes drives v2-routed-to-namespace
// and both hold-label-conventions checks through real BdStores over the
// fixture bd: adding 200 bound routes to the city changes neither the report
// nor the number of bd forks.
func TestDoctorRoutedToForksDoNotScaleWithRoutes(t *testing.T) {
	checks := func(fc *fixtureCity) func(func(string) (beads.Store, error)) []doctor.Check {
		return func(factory func(string) (beads.Store, error)) []doctor.Check {
			return []doctor.Check{
				newV2RoutedToNamespaceCheck(fc.cfg, fc.cityPath, factory),
				newHoldLabelConventionsCheck(fc.cityPath, "city", factory),
				newHoldLabelConventionsCheck(fc.rigPath, "repo", factory),
			}
		}
	}
	run := func(extraRoutes int) (string, int64) {
		fc := newFixtureCity(t, true)
		for i := range extraRoutes {
			fc.cfg.Agents = append(fc.cfg.Agents, config.Agent{Name: fmt.Sprintf("extra%03d", i), BindingName: "gastown"})
		}
		text, _ := fc.runDoctor(t, true, false, checks(fc))
		return text, fc.calls()
	}
	fewText, few := run(0)
	manyText, many := run(200)
	if manyText != fewText {
		t.Fatalf("report changed with unmatched routes:\n--- 200 more ---\n%s\n--- base ---\n%s", manyText, fewText)
	}
	if many != few {
		t.Fatalf("bd forks = %d with 200 more routes, %d without; reads must not scale with routes", many, few)
	}
	// One listing per store, shared by all three checks. (A production work
	// store's policy layer widens it to both tiers, adding one wisp query per
	// store; the fixture's bare BdStore reads the issues tier.)
	if few != 2 {
		t.Fatalf("bd forks = %d, want 2 (one listing per store)", few)
	}
}
