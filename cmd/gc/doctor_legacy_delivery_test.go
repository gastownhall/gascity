package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	beadsexec "github.com/gastownhall/gascity/internal/beads/exec"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// legacyDeliveryFit is a canned answer from the check's sessions-store probe.
type legacyDeliveryFit struct {
	fit     laneStoreFit
	binding string
	err     error
}

// sqliteFit is a split city's sessions store: SQLite with guarded batches.
var sqliteFit = legacyDeliveryFit{fit: laneStoreFitDefault}

// newLegacyDeliveryCheckForTest builds the check over a canned queue, poller
// count, sessions-store probe and lane-status reply.
func newLegacyDeliveryCheckForTest(cfg *config.City, state nudgequeue.State, pollers int, fit legacyDeliveryFit, st conversationLaneStatus, queryErr error) *legacyDeliveryDoctorCheck {
	c := newLegacyDeliveryDoctorCheck(cfg, func() (laneStoreFit, string, error) { return fit.fit, fit.binding, fit.err })
	c.queue = func(string) (nudgequeue.State, error) { return state, nil }
	c.pollers = func(string) (int, error) { return pollers, nil }
	c.query = func(string) (conversationLaneStatus, error) { return st, queryErr }
	return c
}

// TestLegacyDeliveryDoctor pins the legacy-delivery check (OJ-21 21a,
// D-NOLANE): it counts the queue and live pollers, says whether the lane or
// legacy delivery serves the city, warns, advisory, of each deprecation that
// applies where the lane runs or the default is held back, tells a city
// whose sessions store cannot host lanes to split its storage, and notes to
// a city whose store hosts lanes off SQLite that a split is cheaper.
func TestLegacyDeliveryDoctor(t *testing.T) {
	queued := nudgequeue.State{Pending: []nudgequeue.Item{{ID: "n-1"}, {ID: "n-2"}}, Dead: []nudgequeue.Item{{ID: "n-3"}}}
	lane := conversationLaneStatus{State: laneHostStateRunning, Kinds: laneDefaultKinds, KindsDefault: true, DefaultApplies: true}
	off := conversationLaneStatus{State: laneHostStateOff}
	dispatcher := &config.City{}
	dispatcher.Daemon.NudgeDispatcher = "supervisor"
	rollback := &config.City{Ops: config.OpsConfig{Authoritative: config.OpsKinds()}}
	bdFit := legacyDeliveryFit{fit: laneStoreFitNone, binding: "the bd CLI (BdStore)"}
	doltFit := legacyDeliveryFit{fit: laneStoreFitListed}
	splitHint := []string{"docs/runbooks/split-storage-classes.md", "gc storage migrate"}
	for _, tc := range []struct {
		name     string
		cfg      *config.City
		fit      legacyDeliveryFit
		st       conversationLaneStatus
		queryErr error
		status   doctor.CheckStatus
		want     []string
		hint     []string
		details  []string
	}{
		{name: "lane runs, nothing deprecated", cfg: &config.City{}, fit: sqliteFit, st: lane, status: doctor.StatusOK, want: []string{"the conversation lane delivers message, interrupt, respond", "removed in a future release"}},
		{name: "lane runs, dispatcher key set", cfg: dispatcher, fit: sqliteFit, st: lane, status: doctor.StatusWarning, want: []string{"[daemon] nudge_dispatcher is deprecated"}, hint: []string{"keep the deprecated keys until legacy delivery is removed in a future release"}},
		{name: "rollback on a SQLite city", cfg: rollback, fit: sqliteFit, st: conversationLaneStatus{State: laneHostStateRunning, DefaultApplies: true}, status: doctor.StatusWarning, want: []string{"legacy delivery serves this city", "authoritative = [] keeps message, interrupt and respond on legacy delivery"}},
		{name: "unsplit bd city told to split", cfg: dispatcher, fit: bdFit, st: off, status: doctor.StatusWarning, want: []string{"legacy delivery serves this city", "the sessions store cannot host conversation lanes (it is bound to the bd CLI (BdStore))", "legacy delivery is removed in a future release"}, hint: splitHint},
		{name: "unsplit bd city, controller down", cfg: &config.City{}, fit: bdFit, queryErr: errors.New("dial: no such file"), status: doctor.StatusWarning, want: []string{"did not answer lane-status", "cannot host conversation lanes"}, hint: splitHint},
		{name: "native dolt city gets a note", cfg: &config.City{}, fit: doltFit, st: off, status: doctor.StatusOK, want: []string{"legacy delivery serves this city", "splitting this city's storage moves lane records to SQLite, which is cheaper"}, details: []string{"note: splitting this city's storage moves lane records to SQLite, which is cheaper (docs/runbooks/split-storage-classes.md, gc storage migrate)"}},
		{name: "native dolt city listing kinds", cfg: &config.City{Ops: config.OpsConfig{Authoritative: config.OpsKinds("message")}}, fit: doltFit, st: conversationLaneStatus{State: laneHostStateRunning, Kinds: []string{"message"}}, status: doctor.StatusOK, want: []string{"the conversation lane delivers message", "moves lane records to SQLite"}},
		{name: "sessions store unread", cfg: &config.City{}, fit: legacyDeliveryFit{err: errDoctorStoreNotRunning}, st: off, status: doctor.StatusOK, want: []string{"legacy delivery serves this city"}, details: []string{"sessions store: not read (" + errDoctorStoreNotRunning.Error() + ")"}},
		{name: "controller not answering", cfg: &config.City{}, fit: sqliteFit, queryErr: errors.New("dial: no such file"), status: doctor.StatusOK, want: []string{"did not answer lane-status"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newLegacyDeliveryCheckForTest(tc.cfg, queued, 3, tc.fit, tc.st, tc.queryErr)
			r := c.Run(&doctor.CheckContext{CityPath: "/city"})
			if r.Status != tc.status {
				t.Fatalf("status = %v (%q), want %v", r.Status, r.Message, tc.status)
			}
			for _, w := range tc.want {
				if !strings.Contains(r.Message, w) {
					t.Fatalf("message %q, want it to contain %q", r.Message, w)
				}
			}
			for _, h := range tc.hint {
				if !strings.Contains(r.FixHint, h) {
					t.Fatalf("hint %q, want it to contain %q", r.FixHint, h)
				}
			}
			for _, d := range tc.details {
				if !slices.Contains(r.Details, d) {
					t.Fatalf("details = %q, want %q", r.Details, d)
				}
			}
			if tc.status == doctor.StatusWarning && (r.Severity != doctor.SeverityAdvisory || r.FixHint == "") {
				t.Fatalf("warning severity %v hint %q, want advisory with a remedy", r.Severity, r.FixHint)
			}
			if tc.status == doctor.StatusOK && r.FixHint != "" {
				t.Fatalf("OK with hint %q, want none", r.FixHint)
			}
			if strings.Contains(r.FixHint, "remove the deprecated keys") {
				t.Fatalf("hint %q asks to remove keys that still govern legacy delivery", r.FixHint)
			}
			for _, text := range []string{r.Message, r.FixHint} {
				if strings.Contains(text, "next release") || strings.Contains(text, "journal") {
					t.Fatalf("%q promises a next-release removal or a local journal; D-NOLANE removes legacy delivery in a future release, after a split", text)
				}
			}
			if !slices.Contains(r.Details, "nudge queue: 2 pending, 0 in flight, 1 dead") || !slices.Contains(r.Details, "live nudge pollers: 3") {
				t.Fatalf("details = %q, want the queue depth and pollers", r.Details)
			}
		})
	}
	c := newLegacyDeliveryDoctorCheck(&config.City{}, nil)
	if c.Name() != "legacy-delivery" || c.CanFix() || c.WarmupEligible() {
		t.Fatalf("check = %s fix %v warmup %v", c.Name(), c.CanFix(), c.WarmupEligible())
	}
}

// TestConversationLanesSessionsFit pins the probe the legacy-delivery check
// reads: the city store's sessions class, classified by the lane host's own
// predicate (laneStoreFitFor), with the binding of a store that cannot host
// lanes; errLaneStoreMissing without a sessions store; the open failure.
func TestConversationLanesSessionsFit(t *testing.T) {
	bd := beads.NewBdStore(t.TempDir(), func(string, string, ...string) ([]byte, error) {
		return nil, errors.New("the bd CLI must not run")
	})
	for _, tc := range []struct {
		name    string
		store   beads.Store
		fit     laneStoreFit
		binding string
	}{
		{name: "sqlite", store: newLaneKindsSQLiteStore(t), fit: laneStoreFitDefault},
		{name: "batching, not SQLite", store: newLaneTestStore(), fit: laneStoreFitListed},
		{name: "bd", store: bd, fit: laneStoreFitNone, binding: "the bd CLI (BdStore)"},
		{name: "exec", store: beadsexec.NewStore("/bin/false"), fit: laneStoreFitNone, binding: "a store without guarded batches"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, opened := newConversationLanesCheckForTest(nil, tc.store, conversationLaneStatus{}, nil)
			fit, binding, err := c.sessionsFit()
			if err != nil || fit != tc.fit || !strings.HasPrefix(binding, tc.binding) || (tc.binding == "") != (binding == "") {
				t.Fatalf("sessionsFit = %v %q %v, want %v %q", fit, binding, err, tc.fit, tc.binding)
			}
			if *opened != 1 {
				t.Fatalf("store opened %d times, want once", *opened)
			}
		})
	}
	c, _ := newConversationLanesCheckForTest(nil, nil, conversationLaneStatus{}, nil)
	if _, _, err := c.sessionsFit(); !errors.Is(err, errLaneStoreMissing) {
		t.Fatalf("no sessions store: err %v, want errLaneStoreMissing", err)
	}
	c.open = func() (beads.Store, error) { return nil, errors.New("dolt: connection refused") }
	if _, _, err := c.sessionsFit(); err == nil || !strings.Contains(err.Error(), "opening the city store: dolt: connection refused") {
		t.Fatalf("open failure: err %v, want it wrapped", err)
	}
}

// TestLiveCityNudgePollersCountsNone: a city with no pollers directory has
// no live poller, and PID files naming a dead process, garbage or a live
// process that is not this city's poller count none.
func TestLiveCityNudgePollersCountsNone(t *testing.T) {
	city := t.TempDir()
	if n, err := liveCityNudgePollers(city); err != nil || n != 0 {
		t.Fatalf("no pollers dir: %d, %v; want 0", n, err)
	}
	for i, content := range []string{fmt.Sprintf("%d\n", deadPID(t)), "not-a-pid\n", fmt.Sprintf("%d\n", os.Getpid())} {
		pidPath := nudgePollerPIDPath(city, fmt.Sprintf("sess-%d", i), "session-id")
		if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(pidPath, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	if n, err := liveCityNudgePollers(city); err != nil || n != 0 {
		t.Fatalf("no live poller: %d, %v; want 0", n, err)
	}
}
