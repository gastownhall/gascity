package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestSendParkedWorkMailLandsInMayorInbox drives the controller's real park
// mailer (workStartFailurePolicy.notify as city_runtime wires it) against an
// in-memory city: the mail is addressed to the configured mayor through the
// same recipient resolution gc mail send uses, persists as a message bead in
// the messaging store, and reads back from the mayor's inbox.
func TestSendParkedWorkMailLandsInMayorInbox(t *testing.T) {
	store := beads.NewMemStore()
	var stderr bytes.Buffer
	cr := &CityRuntime{
		cityPath: t.TempDir(),
		cityName: "test-city",
		cfg: &config.City{
			Workspace:     config.Workspace{Name: "test-city"},
			Agents:        []config.Agent{{Name: "mayor", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(1)}, {Name: "worker", MaxActiveSessions: intPtr(2)}},
			NamedSessions: []config.NamedSession{{Name: "mayor", Template: "mayor"}},
		},
		sp:            runtime.NewFake(),
		rec:           events.Discard,
		stderr:        &stderr,
		storageRoutes: messagingSplitRoutes(store),
	}
	policy := cr.workStartFailurePolicy(store, store, nil)
	if got := policy.limit("worker"); got != config.DefaultMaxStartFailures {
		t.Fatalf("limit for an agent without max_start_failures = %d, want %d", got, config.DefaultMaxStartFailures)
	}
	notice := parkedWorkNotice{
		BeadID:   "gp-fh5f",
		Title:    "the bead whose pre_start could not pick a branch",
		Agent:    "gascity-packs/implementation-worker-codex",
		Failures: 5,
		Reason:   "worker-worktree: ERROR several branches name bead gp-fh5f; pass --bead with a unique id, or --base and no bead: gp-fh5f gp-fh5f-upstream",
		ParkedAt: time.Date(2026, 9, 11, 3, 4, 0, 0, time.UTC),
		ParkID:   "0123456789abcdef",
	}
	if found, err := policy.lookup(notice); err != nil || found {
		t.Fatalf("lookup before any send: found=%v err=%v", found, err)
	}
	if err := policy.notify(notice); err != nil {
		t.Fatalf("notify: %v (stderr: %s)", err, stderr.String())
	}
	inbox, err := newMailProviderWithSessionStore(store, store).Inbox("mayor")
	if err != nil {
		t.Fatalf("Inbox(mayor): %v", err)
	}
	if len(inbox) != 1 {
		t.Fatalf("mayor inbox has %d messages, want 1", len(inbox))
	}
	m := inbox[0]
	if m.From != controllerMailIdentity || m.To != "mayor" {
		t.Fatalf("from=%q to=%q, want %q → mayor", m.From, m.To, controllerMailIdentity)
	}
	if m.Subject != notice.Subject() || !strings.Contains(m.Subject, "PARKED gp-fh5f") {
		t.Fatalf("subject = %q", m.Subject)
	}
	for _, want := range []string{"gp-fh5f", "gascity-packs/implementation-worker-codex", "5 consecutive", "several branches name bead gp-fh5f", "gc sling --reassign <agent> gp-fh5f", "--unset-metadata gc.park_reason --unset-metadata gc.parked_at --unset-metadata gc.park_failures"} {
		if !strings.Contains(m.Body, want) {
			t.Fatalf("body must name %q:\n%s", want, m.Body)
		}
	}
	if !strings.Contains(stderr.String(), "park mail "+m.ID+" sent to mayor for work bead gp-fh5f") {
		t.Fatalf("the send must be said on stderr:\n%s", stderr.String())
	}
	if !strings.Contains(m.Subject, "[park 0123456789abcdef]") {
		t.Fatalf("the subject must carry the park tag: %q", m.Subject)
	}
	if found, err := policy.lookup(notice); err != nil || !found {
		t.Fatalf("lookup after the send must find the mail by its tag: found=%v err=%v", found, err)
	}
	if found, _ := policy.lookup(parkedWorkNotice{BeadID: "gp-fh5f", ParkID: "other"}); found {
		t.Fatal("lookup must not match another park's tag")
	}
	// The mayor archives the mail (a restart lost the stamp in between): the
	// receipt is the message bead, closed, not gone — the lookup still finds it.
	if err := newMailProviderWithSessionStore(store, store).Archive(m.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if open, err := newMailProviderWithSessionStore(store, store).All("mayor"); err != nil || len(open) != 0 {
		t.Fatalf("All after Archive: %d messages, err=%v; want none open", len(open), err)
	}
	if found, err := policy.lookup(notice); err != nil || !found {
		t.Fatalf("lookup after the mayor archived the mail must still find it: found=%v err=%v", found, err)
	}
	t.Logf("park mail sample:\nFrom: %s\nTo: %s\nSubject: %s\n\n%s", m.From, m.To, m.Subject, m.Body)
}

// TestSendParkedWorkMailWithoutAMayorIsAnError: a city with no mayor agent
// cannot be mailed; the park stays unmailed (retried, throttled) and the
// reason is said.
func TestSendParkedWorkMailWithoutAMayorIsAnError(t *testing.T) {
	store := beads.NewMemStore()
	var stderr bytes.Buffer
	cr := &CityRuntime{
		cityPath:      t.TempDir(),
		cityName:      "test-city",
		cfg:           &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{{Name: "worker"}}},
		sp:            runtime.NewFake(),
		rec:           events.Discard,
		stderr:        &stderr,
		storageRoutes: messagingSplitRoutes(store),
	}
	policy := cr.workStartFailurePolicy(store, store, nil)
	err := policy.notify(parkedWorkNotice{BeadID: "gp-1", Agent: "worker", Failures: 5, Reason: "boom", ParkedAt: time.Now()})
	if err == nil || !strings.Contains(err.Error(), `resolving recipient "mayor"`) {
		t.Fatalf("notify without a mayor = %v, want a recipient resolution error", err)
	}
	if inbox, _ := newMailProviderWithSessionStore(store, store).Inbox("mayor"); len(inbox) != 0 {
		t.Fatalf("no mayor, yet %d messages landed", len(inbox))
	}
	// The retry throttle: a second attempt inside parkMailRetryEvery is not
	// due; the park's own first attempt never is throttled.
	if !policy.retry.due("gp-1", time.Now()) {
		t.Fatal("first attempt must be due")
	}
	policy.retry.mark("gp-1", time.Now())
	if policy.retry.due("gp-1", time.Now().Add(parkMailRetryEvery/2)) {
		t.Fatal("a retry inside the throttle window must not be due")
	}
	if !policy.retry.due("gp-1", time.Now().Add(parkMailRetryEvery+time.Second)) {
		t.Fatal("a retry after the throttle window must be due")
	}
}

// TestPoolStartBackoffArchivedParkMailIsNotResentAfterARestart: the park mail
// lands, the controller dies before gc.park_mailed_at persists, and the mayor
// archives the mail before the restarted controller's first retry. The
// receipt is the message bead itself — Archive closes it, never deletes it —
// so the retry finds the landed mail by its tag among archived mail, stamps
// the park and sends nothing: one park, one mail, whatever the mayor did with
// it. This drives the real mailer, the real lookup and the real retry.
func TestPoolStartBackoffArchivedParkMailIsNotResentAfterARestart(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	store := beads.NewMemStore()
	var stderr bytes.Buffer
	cr := &CityRuntime{
		cityPath: t.TempDir(),
		cityName: "test-city",
		cfg: &config.City{
			Workspace:     config.Workspace{Name: "test-city"},
			Rigs:          []config.Rig{{Name: "riga", Path: "riga"}, {Name: "rigb", Path: "rigb"}},
			Agents:        []config.Agent{{Name: "mayor", Dir: "riga", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(1)}, {Name: "worker", MaxActiveSessions: intPtr(2)}},
			NamedSessions: []config.NamedSession{{Name: "mayor", Template: "mayor", Dir: "riga"}},
		},
		sp:            runtime.NewFake(),
		rec:           events.Discard,
		stderr:        &stderr,
		storageRoutes: messagingSplitRoutes(store),
	}
	makeMayor := func(identity string) beads.Bead {
		row, err := store.Create(beads.Bead{Title: identity, Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
			"template": identity, "alias": identity, "agent_name": identity, "session_name": strings.ReplaceAll(identity, "/", "-"), "state": "asleep", "configured_named_session": "true", "configured_named_identity": identity,
		}})
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	oldMayor := makeMayor("riga/mayor")
	work, err := store.Create(beads.Bead{
		Title: "routed work the pool parked",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:     "worker",
			beadmeta.ParkedAtMetadataKey:     "2026-09-11T03:04:00Z",
			beadmeta.ParkReasonMetadataKey:   "pre_start[0]: exit status 1",
			beadmeta.ParkFailuresMetadataKey: "5",
			beadmeta.ParkIDMetadataKey:       "0123456789abcdef",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	reload := func() beads.Bead {
		b, err := store.Get(work.ID)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	mp := newMailProviderWithSessionStore(store, store)
	archived, ok := mp.(mail.ArchivedLister)
	if !ok {
		t.Fatal("the city mail provider must keep a record of archived mail")
	}

	policy := cr.workStartFailurePolicy(store, store, nil)
	policy.retryUnmailedParks([]beads.Bead{reload()}, []string{"city"})
	policy.awaitParkMailRetries()
	open, err := mp.All("riga/mayor")
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("after the first retry the mayor has %d park mails, want 1 (stderr: %s)", len(open), stderr.String())
	}
	if readWorkStartFailureState(reload().Metadata).ParkMailedAt.IsZero() {
		t.Fatal("the landed mail must be stamped")
	}

	// The controller died before the stamp persisted; the mayor dismissed the mail.
	if err := store.SetMetadataBatch(work.ID, map[string]string{beadmeta.ParkMailedAtMetadataKey: ""}); err != nil {
		t.Fatal(err)
	}
	if err := mp.Archive(open[0].ID); err != nil {
		t.Fatal(err)
	}
	if still, err := mp.All("riga/mayor"); err != nil || len(still) != 0 {
		t.Fatalf("All after Archive = %d, err=%v; want no open mail", len(still), err)
	}

	if err := store.Close(oldMayor.ID); err != nil {
		t.Fatal(err)
	}
	makeMayor("rigb/mayor")
	cr.cfg.Agents[0].Dir = "rigb"
	cr.cfg.NamedSessions[0].Dir = "rigb"
	cr.parkMailRetry = nil // the restart: no in-memory throttle survives it
	policy = cr.workStartFailurePolicy(store, store, nil)
	policy.retryUnmailedParks([]beads.Bead{reload()}, []string{"city"})
	policy.awaitParkMailRetries()

	all, err := archived.AllIncludingArchived("")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("after the restart's retry the mayor has %d park mails (open or archived), want only the one that landed (stderr: %s)", len(all), stderr.String())
	}
	if all[0].ID != open[0].ID {
		t.Fatalf("the surviving mail is %s, want the landed one %s", all[0].ID, open[0].ID)
	}
	if readWorkStartFailureState(reload().Metadata).ParkMailedAt.IsZero() {
		t.Fatal("the found mail must be stamped after the restart")
	}
	if !strings.Contains(stderr.String(), "already landed; stamping without a second send") {
		t.Fatalf("stderr must say the mail was found:\n%s", stderr.String())
	}
}

// A diagnostic containing the park tag is not the controller's delivery receipt.
func TestParkMailReceiptRequiresControllerSender(t *testing.T) {
	store := beads.NewMemStore()
	var stderr bytes.Buffer
	cr := &CityRuntime{
		cityPath: t.TempDir(), cityName: "test-city",
		cfg: &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{{Name: "mayor", MaxActiveSessions: intPtr(1)}, {Name: "worker", MaxActiveSessions: intPtr(1)}}, NamedSessions: []config.NamedSession{{Name: "mayor", Template: "mayor"}}},
		sp:  runtime.NewFake(), rec: events.Discard, stderr: &stderr, storageRoutes: messagingSplitRoutes(store),
	}
	work, err := store.Create(beads.Bead{Title: "parked work", Type: "task", Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey: "worker", beadmeta.ParkedAtMetadataKey: "2026-09-11T03:04:00Z", beadmeta.ParkFailuresMetadataKey: "5", beadmeta.ParkIDMetadataKey: "controller-receipt", beadmeta.ParkReasonMetadataKey: "pre_start failed",
	}})
	if err != nil {
		t.Fatal(err)
	}
	mp := newMailProviderWithSessionStore(store, store)
	if _, err := mp.Send("worker", "mayor", "Investigating [park controller-receipt]", "A diagnostic, not the park alert"); err != nil {
		t.Fatal(err)
	}
	policy := cr.workStartFailurePolicy(store, store, nil)
	policy.retryUnmailedParks([]beads.Bead{work}, []string{"city"})
	policy.awaitParkMailRetries()
	all, err := mp.All("mayor")
	if err != nil {
		t.Fatal(err)
	}
	controllerMails := 0
	for _, m := range all {
		if m.From == controllerMailIdentity {
			controllerMails++
		}
	}
	if controllerMails != 1 {
		t.Fatalf("diagnostic must not suppress the controller alert: got %d controller messages\nstderr: %s", controllerMails, stderr.String())
	}
	row, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if readWorkStartFailureState(row.Metadata).ParkMailedAt.IsZero() {
		t.Fatal("controller delivery must be stamped")
	}
}
