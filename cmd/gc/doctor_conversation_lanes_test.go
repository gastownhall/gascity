package main

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// newConversationLanesCheckForTest builds the check over store and a canned
// lane-status answer.
func newConversationLanesCheckForTest(kinds []string, store beads.Store, st conversationLaneStatus, queryErr error) (*conversationLanesDoctorCheck, *int) {
	opened := 0
	c := &conversationLanesDoctorCheck{
		cfg: &config.City{Ops: config.OpsConfig{Authoritative: kinds}},
		open: func() (beads.Store, error) {
			opened++
			return store, nil
		},
		sessions: func(s beads.Store) beads.Store { return s },
		query:    func(string) (conversationLaneStatus, error) { return st, queryErr },
	}
	return c, &opened
}

// TestConversationLanesDoctorStates pins every state the check reports.
func TestConversationLanesDoctorStates(t *testing.T) {
	mem := newLaneTestStore()
	bd := beads.NewBdStore(t.TempDir(), func(string, string, ...string) ([]byte, error) {
		return nil, errors.New("the bd CLI must not run")
	})
	running := conversationLaneStatus{State: laneHostStateRunning, Kinds: []string{"message"}, Passes: 3, LastPassAt: "2026-10-16T12:00:00Z", LastPassAgeMS: 4000, Workers: 1}
	quarantined := running
	quarantined.Quarantined, quarantined.QuarantinedIDs = 2, []string{"gc-op-a", "gc-op-b"}
	stale := running
	stale.Kinds = nil
	failing := running
	failing.LastPassFailed, failing.ErrorsSinceSuccess, failing.LastError = true, 3, "probing conversation gc-1: runtime exploded"
	failing.LastSuccessAt, failing.LastSuccessAgeMS = "2026-10-16T11:00:00Z", 3600000
	scanFailing := running
	scanFailing.ErrorsSinceSuccess = 2
	for _, tc := range []struct {
		name     string
		kinds    []string
		store    beads.Store
		openErr  error
		st       conversationLaneStatus
		queryErr error
		status   doctor.CheckStatus
		want     string
		opens    int
	}{
		{name: "off", status: doctor.StatusOK, want: "conversation lanes are off", opens: 0},
		{name: "off, controller not answering", queryErr: errors.New("dial: no such file"), status: doctor.StatusOK, want: "conversation lanes are off", opens: 0},
		{name: "off, host runs no list", st: conversationLaneStatus{State: laneHostStateRunning}, status: doctor.StatusOK, want: "conversation lanes are off", opens: 0},
		{name: "off, host refused its list", st: conversationLaneStatus{State: laneHostStateRefused, Kinds: []string{"message"}}, status: doctor.StatusOK, want: "conversation lanes are off", opens: 0},
		{name: "emptied while serving", st: running, status: doctor.StatusWarning, want: "conversation lanes are off in city.toml, but the controller runs [ops] authoritative = [message] until its next start", opens: 0},
		{name: "emptied while waiting for its store", st: conversationLaneStatus{State: laneHostStateUnavailable, Kinds: []string{"message"}}, status: doctor.StatusWarning, want: "until its next start", opens: 0},
		{
			name: "incapable store", kinds: []string{"message"}, store: bd, status: doctor.StatusError,
			want: "conversation lanes need guarded batches; this city's sessions class is bound to the bd CLI (BdStore)", opens: 1,
		},
		{name: "store does not open", kinds: []string{"message"}, openErr: errors.New("dolt: connection refused"), st: running, status: doctor.StatusWarning, want: "opening the city store failed: dolt: connection refused", opens: 1},
		{name: "no sessions store", kinds: []string{"message"}, st: running, status: doctor.StatusWarning, want: "the city store has no sessions store", opens: 1},
		{name: "no reply", kinds: []string{"message"}, store: mem, queryErr: errors.New("dial: no such file"), status: doctor.StatusWarning, want: "no lane-status reply", opens: 1},
		{name: "no host", kinds: []string{"message"}, store: mem, st: conversationLaneStatus{State: laneHostStateNone}, status: doctor.StatusError, want: "runs no lane host for this city, so the listed kinds go unserved", opens: 1},
		{name: "refused", kinds: []string{"message"}, store: mem, st: conversationLaneStatus{State: laneHostStateRefused, Reason: "conversation lanes need guarded batches"}, status: doctor.StatusError, want: "refused to start the lane host", opens: 1},
		{name: "running", kinds: []string{"message"}, store: mem, st: running, status: doctor.StatusOK, want: "last pass 4s ago", opens: 1},
		{name: "quarantined", kinds: []string{"message"}, store: mem, st: quarantined, status: doctor.StatusWarning, want: "2 op row(s) this gc cannot decode", opens: 1},
		{name: "list changed since start", kinds: []string{"message"}, store: mem, st: stale, status: doctor.StatusWarning, want: "until its next start", opens: 1},
		{name: "last pass failed", kinds: []string{"message"}, store: mem, st: failing, status: doctor.StatusWarning, want: "last pass 4s ago, last success 1h0m0s ago: the last pass failed", opens: 1},
		{name: "failures since success", kinds: []string{"message"}, store: mem, st: scanFailing, status: doctor.StatusWarning, want: "2 failure(s) since the last pass that succeeded", opens: 1},
		{name: "store missing", kinds: []string{"message"}, store: mem, st: conversationLaneStatus{State: laneHostStateUnavailable, Reason: errLaneStoreMissing.Error()}, status: doctor.StatusWarning, want: "waits for the controller's sessions store", opens: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, opened := newConversationLanesCheckForTest(tc.kinds, tc.store, tc.st, tc.queryErr)
			if tc.openErr != nil {
				c.open = func() (beads.Store, error) {
					*opened++
					return nil, tc.openErr
				}
			}
			r := c.Run(&doctor.CheckContext{CityPath: "/city"})
			if r.Status != tc.status || !strings.Contains(r.Message, tc.want) {
				t.Fatalf("Run = %v %q, want %v containing %q", r.Status, r.Message, tc.status, tc.want)
			}
			if *opened != tc.opens {
				t.Fatalf("store opened %d times, want %d", *opened, tc.opens)
			}
			if tc.status == doctor.StatusError && r.FixHint == "" {
				t.Fatal("an error carries no remedy")
			}
		})
	}
}

// TestConversationLanesDoctorHints pins the no-host remedy to a restart of
// the city, whose controller then starts the lane host, the emptied-list
// remedy to a restart, and the waits a running host reports to its details.
func TestConversationLanesDoctorHints(t *testing.T) {
	c, _ := newConversationLanesCheckForTest([]string{"message"}, newLaneTestStore(), conversationLaneStatus{State: laneHostStateNone}, nil)
	const want = "the running controller predates the lane host; restart the city (`gc stop`, then `gc start`), or empty [ops] authoritative"
	if r := c.Run(&doctor.CheckContext{CityPath: "/city"}); r.FixHint != want {
		t.Fatalf("no-host hint = %q, want %q", r.FixHint, want)
	}
	c, _ = newConversationLanesCheckForTest(nil, nil, conversationLaneStatus{State: laneHostStateRunning, Kinds: []string{"message"}}, nil)
	if r := c.Run(&doctor.CheckContext{CityPath: "/city"}); r.FixHint != "restart the controller to apply the emptied list" || r.Severity != doctor.SeverityAdvisory {
		t.Fatalf("emptied-list hint = %q severity %v, want the restart remedy, advisory", r.FixHint, r.Severity)
	}
	st := conversationLaneStatus{State: laneHostStateRunning, Kinds: []string{"message"}, Waiting: []laneConversationWaits{{
		Conversation: "gc-1", Wake: "held",
		Waits: []laneOpWaitNote{{Op: "gc-op-1", Reason: "content_block", Detail: "held"}},
	}}}
	c, _ = newConversationLanesCheckForTest([]string{"message"}, newLaneTestStore(), st, nil)
	r := c.Run(&doctor.CheckContext{CityPath: "/city"})
	if r.Status != doctor.StatusOK || !slices.Contains(r.Details, "conversation gc-1 waits: gc-op-1 content_block (held); wake held") {
		t.Fatalf("Run = %v details %q, want OK with the wait detail", r.Status, r.Details)
	}
}

// TestConversationLanesDoctorNotFixable pins the check's identity.
func TestConversationLanesDoctorNotFixable(t *testing.T) {
	c := newConversationLanesDoctorCheck("/city", &config.City{})
	if c.Name() != "conversation-lanes" || c.CanFix() || c.WarmupEligible() {
		t.Fatalf("check = %s fix %v warmup %v", c.Name(), c.CanFix(), c.WarmupEligible())
	}
}

// TestConversationLanesDoctorWarnsMessageUnderV2 pins ga-f35wnp's visible
// half: message listed under the v2 reconciler warns, whatever the host's
// state; nothing warns under legacy or with the list empty.
func TestConversationLanesDoctorWarnsMessageUnderV2(t *testing.T) {
	running := conversationLaneStatus{State: laneHostStateRunning, Kinds: []string{"message"}, LastPassAt: "2026-10-16T12:00:00Z", LastPassAgeMS: 1000}
	for _, tc := range []struct {
		name       string
		kinds      []string
		reconciler string
		st         conversationLaneStatus
		status     doctor.CheckStatus
		warns      bool
	}{
		{name: "v2 with message", kinds: []string{"message"}, reconciler: "v2", st: running, status: doctor.StatusWarning, warns: true},
		{name: "v2 with message, host missing", kinds: []string{"message"}, reconciler: "v2", st: conversationLaneStatus{State: laneHostStateNone}, status: doctor.StatusError, warns: true},
		{name: "legacy with message", kinds: []string{"message"}, st: running, status: doctor.StatusOK},
		{name: "v2 with the list empty", reconciler: "v2", status: doctor.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newConversationLanesCheckForTest(tc.kinds, newLaneTestStore(), tc.st, nil)
			c.cfg.Daemon.SessionReconciler = tc.reconciler
			c.lookupEnv = func(string) (string, bool) { return "", false }
			r := c.Run(&doctor.CheckContext{CityPath: "/city"})
			if r.Status != tc.status || strings.Contains(r.Message, laneMessageUnderV2Warning) != tc.warns {
				t.Fatalf("Run = %v %q, want %v, warning %v", r.Status, r.Message, tc.status, tc.warns)
			}
			if tc.warns && tc.status == doctor.StatusWarning && r.Severity != doctor.SeverityAdvisory {
				t.Fatalf("severity %v, want advisory", r.Severity)
			}
		})
	}
}
