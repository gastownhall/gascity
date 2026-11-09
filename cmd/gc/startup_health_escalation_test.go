package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/session"
)

const escalationTestRecipient = "ops/oncall"

var escalationTestNow = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

// countingMail records every Send attempt, successful or not.
type countingMail struct {
	mail.Provider
	mu    sync.Mutex
	calls int
	err   error
}

func (c *countingMail) Send(from, to, subject, body string) (mail.Message, error) {
	c.mu.Lock()
	c.calls++
	err := c.err
	c.mu.Unlock()
	if err != nil {
		return mail.Message{}, err
	}
	return c.Provider.Send(from, to, subject, body)
}

func (c *countingMail) attempts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// flakyStore fails the nth SetMetadataBatch call (1-based) once.
type flakyStore struct {
	beads.Store
	mu     sync.Mutex
	calls  int
	failOn int
}

func (f *flakyStore) SetMetadataBatch(id string, kvs map[string]string) error {
	f.mu.Lock()
	f.calls++
	fail := f.calls == f.failOn
	f.mu.Unlock()
	if fail {
		return errors.New("state write refused")
	}
	return f.Store.SetMetadataBatch(id, kvs)
}

type escalationFixture struct {
	store    *session.Store
	mail     *countingMail
	inbox    *mail.Fake
	esc      *startupHealthEscalator
	stderr   *bytes.Buffer
	cityPath string
}

func newEscalationFixture(t *testing.T, to string) *escalationFixture {
	t.Helper()
	return newEscalationFixtureOn(t, to, beads.NewMemStore())
}

func newEscalationFixtureOn(t *testing.T, to string, bs beads.Store) *escalationFixture {
	t.Helper()
	fx := &escalationFixture{
		store:    sessionFrontDoor(bs),
		inbox:    mail.NewFake(),
		stderr:   &bytes.Buffer{},
		cityPath: t.TempDir(),
	}
	fx.mail = &countingMail{Provider: fx.inbox}
	fx.esc = fx.newEscalator(to)
	return fx
}

// newEscalator builds a fresh escalator over the fixture's durable store, the
// way a restarted controller would.
func (fx *escalationFixture) newEscalator(to string) *startupHealthEscalator {
	return &startupHealthEscalator{
		store:    fx.store,
		mail:     fx.mail,
		to:       to,
		cityPath: fx.cityPath,
		now:      func() time.Time { return escalationTestNow },
		stderr:   fx.stderr,
	}
}

func (fx *escalationFixture) seed(t *testing.T, ep session.StartupHealthEpisode) {
	t.Helper()
	if err := fx.store.SaveStartupHealthEpisode(ep); err != nil {
		t.Fatalf("seeding episode: %v", err)
	}
}

func (fx *escalationFixture) disposition(t *testing.T) session.AlertDisposition {
	t.Helper()
	ep, err := fx.store.LoadStartupHealthEpisode("s1")
	if err != nil {
		t.Fatalf("loading episode: %v", err)
	}
	return ep.AlertDisposition
}

func (fx *escalationFixture) writeStderr(t *testing.T, content string) {
	t.Helper()
	dir := filepath.Join(citylayout.SessionDiagnosticsDir(fx.cityPath), "s1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "start-stderr.log"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func pendingEpisode(name string, kind session.FailureKind) session.StartupHealthEpisode {
	return session.StartupHealthEpisode{
		SessionName:      name,
		ConsecutiveCount: 3,
		FirstFailureAt:   escalationTestNow.Add(-30 * time.Minute),
		LastFailureAt:    escalationTestNow.Add(-time.Minute),
		LastDetail:       "start failed",
		Kind:             kind,
		AlertDisposition: session.AlertDispositionPending,
		QuarantinedUntil: escalationTestNow.Add(time.Hour),
	}
}

func (fx *escalationFixture) onlyMessage(t *testing.T) mail.Message {
	t.Helper()
	msgs := fx.inbox.Messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want exactly 1", len(msgs))
	}
	return msgs[0]
}

func TestStartupHealthEscalationBelowThresholdIsSilent(t *testing.T) {
	fx := newEscalationFixture(t, escalationTestRecipient)
	ep := pendingEpisode("s1", session.FailureKindOther)
	ep.ConsecutiveCount = 2
	ep.AlertDisposition = session.AlertDispositionNone
	fx.seed(t, ep)

	fx.esc.sweep()

	if fx.mail.attempts() != 0 {
		t.Fatalf("send attempts = %d, want 0", fx.mail.attempts())
	}
	if got := fx.disposition(t); got != session.AlertDispositionNone {
		t.Fatalf("disposition = %q, want none", got)
	}
	if fx.stderr.Len() != 0 {
		t.Fatalf("unexpected log output: %q", fx.stderr.String())
	}
}

func TestStartupHealthEscalationSendsOnceAtThreshold(t *testing.T) {
	fx := newEscalationFixture(t, escalationTestRecipient)
	fx.seed(t, pendingEpisode("s1", session.FailureKindTimeout))

	fx.esc.sweep()

	msg := fx.onlyMessage(t)
	if msg.To != escalationTestRecipient {
		t.Errorf("To = %q, want exactly %q", msg.To, escalationTestRecipient)
	}
	if msg.From != startupHealthEscalationSender {
		t.Errorf("From = %q, want SDK identity %q", msg.From, startupHealthEscalationSender)
	}
	if !strings.Contains(msg.Subject, "s1") {
		t.Errorf("Subject %q does not name the session", msg.Subject)
	}
	for _, want := range []string{
		"s1",
		"3",
		"timeout",
		escalationTestNow.Add(-30 * time.Minute).Format(time.RFC3339),
		escalationTestNow.Add(-time.Minute).Format(time.RFC3339),
	} {
		if !strings.Contains(msg.Body, want) {
			t.Errorf("Body missing %q:\n%s", want, msg.Body)
		}
	}
	if got := fx.disposition(t); got != session.AlertDispositionSent {
		t.Fatalf("disposition = %q, want sent", got)
	}
}

func TestStartupHealthEscalationLaterTicksDoNotResend(t *testing.T) {
	fx := newEscalationFixture(t, escalationTestRecipient)
	fx.seed(t, pendingEpisode("s1", session.FailureKindOther))

	for i := 0; i < 5; i++ {
		fx.esc.sweep()
	}

	if got := fx.mail.attempts(); got != 1 {
		t.Fatalf("send attempts = %d, want 1", got)
	}
}

func TestStartupHealthEscalationControllerRestartDoesNotResend(t *testing.T) {
	fx := newEscalationFixture(t, escalationTestRecipient)
	fx.seed(t, pendingEpisode("s1", session.FailureKindOther))
	fx.esc.sweep()

	restarted := fx.newEscalator(escalationTestRecipient)
	restarted.sweep()
	restarted.sweep()

	if got := fx.mail.attempts(); got != 1 {
		t.Fatalf("send attempts across restart = %d, want 1", got)
	}
}

// A controller that dies between claiming the escalation and recording its
// outcome leaves the episode in sending. The claim is written before the
// send, so a restart must not send again.
func TestStartupHealthEscalationRestartAfterClaimDoesNotResend(t *testing.T) {
	fx := newEscalationFixture(t, escalationTestRecipient)
	ep := pendingEpisode("s1", session.FailureKindOther)
	ep.AlertDisposition = session.AlertDispositionSending
	fx.seed(t, ep)

	fx.newEscalator(escalationTestRecipient).sweep()

	if got := fx.mail.attempts(); got != 0 {
		t.Fatalf("send attempts = %d, want 0 for an already-claimed episode", got)
	}
	if got := fx.disposition(t); got != session.AlertDispositionSending {
		t.Fatalf("disposition = %q, want sending left visible for doctor", got)
	}
}

func TestStartupHealthEscalationRecoveryRearmsWithoutRecoveryMail(t *testing.T) {
	fx := newEscalationFixture(t, escalationTestRecipient)
	fx.seed(t, pendingEpisode("s1", session.FailureKindOther))
	fx.esc.sweep()
	if fx.mail.attempts() != 1 {
		t.Fatalf("setup: attempts = %d, want 1", fx.mail.attempts())
	}

	// A successful start clears the episode.
	fx.seed(t, session.ClearStartupHealthEpisode("s1"))
	fx.esc.sweep()
	if got := fx.mail.attempts(); got != 1 {
		t.Fatalf("attempts after recovery = %d, want 1 (no recovery mail)", got)
	}

	// A later episode crossing the threshold escalates again.
	later := pendingEpisode("s1", session.FailureKindOther)
	later.FirstFailureAt = escalationTestNow.Add(-time.Minute)
	fx.seed(t, later)
	fx.esc.sweep()
	fx.esc.sweep()
	if got := fx.mail.attempts(); got != 2 {
		t.Fatalf("attempts after later episode = %d, want 2", got)
	}
}

func TestStartupHealthEscalationEmptyRecipientSkipsAndLogsOnce(t *testing.T) {
	fx := newEscalationFixture(t, "")
	fx.seed(t, pendingEpisode("s1", session.FailureKindOther))

	fx.esc.sweep()
	logged := fx.stderr.String()
	fx.esc.sweep()
	fx.esc.sweep()

	if fx.mail.attempts() != 0 {
		t.Fatalf("send attempts = %d, want 0", fx.mail.attempts())
	}
	if got := fx.disposition(t); got != session.AlertDispositionSkipped {
		t.Fatalf("disposition = %q, want skipped", got)
	}
	if !strings.Contains(logged, "s1") || !strings.Contains(logged, "skipped") {
		t.Fatalf("threshold crossing not logged as skipped: %q", logged)
	}
	if fx.stderr.String() != logged {
		t.Fatalf("later ticks logged again (per-tick storm): %q", fx.stderr.String())
	}
}

func TestStartupHealthEscalationInvalidRecipientFailsWithoutSending(t *testing.T) {
	for _, to := range []string{"ops oncall", "ops\noncall", "ops\x00oncall"} {
		t.Run(strings.NewReplacer("\n", `\n`, "\x00", `\x00`).Replace(to), func(t *testing.T) {
			fx := newEscalationFixture(t, to)
			fx.seed(t, pendingEpisode("s1", session.FailureKindOther))

			fx.esc.sweep()
			fx.esc.sweep()

			if fx.mail.attempts() != 0 {
				t.Fatalf("send attempts = %d, want 0", fx.mail.attempts())
			}
			if got := fx.disposition(t); got != session.AlertDispositionFailed {
				t.Fatalf("disposition = %q, want failed", got)
			}
			if !strings.Contains(fx.stderr.String(), "invalid") {
				t.Fatalf("log does not say the recipient is invalid: %q", fx.stderr.String())
			}
		})
	}
}

func TestStartupHealthEscalationMailFailureIsRecordedNotRetriedPerTick(t *testing.T) {
	fx := newEscalationFixture(t, escalationTestRecipient)
	fx.mail.err = errors.New("mail backend down")
	fx.seed(t, pendingEpisode("s1", session.FailureKindOther))

	fx.esc.sweep()
	fx.esc.sweep()
	fx.esc.sweep()

	if got := fx.mail.attempts(); got != 1 {
		t.Fatalf("send attempts = %d, want 1 (no per-tick retry)", got)
	}
	if got := fx.disposition(t); got != session.AlertDispositionFailed {
		t.Fatalf("disposition = %q, want failed", got)
	}
	if !strings.Contains(fx.stderr.String(), "mail backend down") {
		t.Fatalf("failure not logged with its cause: %q", fx.stderr.String())
	}
}

func TestStartupHealthEscalationUnavailableMailProviderFails(t *testing.T) {
	fx := newEscalationFixture(t, escalationTestRecipient)
	fx.esc.mail = nil
	fx.seed(t, pendingEpisode("s1", session.FailureKindOther))

	fx.esc.sweep()

	if got := fx.disposition(t); got != session.AlertDispositionFailed {
		t.Fatalf("disposition = %q, want failed", got)
	}
	if !strings.Contains(fx.stderr.String(), "mail provider") {
		t.Fatalf("log does not name the missing provider: %q", fx.stderr.String())
	}
}

// When the claim cannot be written nothing is sent: the send follows the
// durable claim, so a store outage cannot produce an unrecorded mail.
func TestStartupHealthEscalationClaimWriteFailureSendsNothingAndRetriesLater(t *testing.T) {
	flaky := &flakyStore{Store: beads.NewMemStore(), failOn: 1}
	fx := newEscalationFixtureOn(t, escalationTestRecipient, flaky)
	fx.seed(t, pendingEpisode("s1", session.FailureKindOther))
	flaky.mu.Lock()
	flaky.calls = 0
	flaky.mu.Unlock()

	fx.esc.sweep()
	if fx.mail.attempts() != 0 {
		t.Fatalf("send attempts = %d, want 0 when the claim write fails", fx.mail.attempts())
	}
	if got := fx.disposition(t); got != session.AlertDispositionPending {
		t.Fatalf("disposition = %q, want pending left for the next tick", got)
	}
	if !strings.Contains(fx.stderr.String(), "state write refused") {
		t.Fatalf("claim failure not logged: %q", fx.stderr.String())
	}

	fx.esc.sweep()
	if got := fx.mail.attempts(); got != 1 {
		t.Fatalf("send attempts after store recovers = %d, want 1", got)
	}
}

// When the mail went out but the outcome cannot be written, the episode stays
// sending, so a later tick or restart cannot send a second copy.
func TestStartupHealthEscalationOutcomeWriteFailureNeverResends(t *testing.T) {
	flaky := &flakyStore{Store: beads.NewMemStore(), failOn: 2}
	fx := newEscalationFixtureOn(t, escalationTestRecipient, flaky)
	fx.seed(t, pendingEpisode("s1", session.FailureKindOther))
	flaky.mu.Lock()
	flaky.calls = 0
	flaky.mu.Unlock()

	fx.esc.sweep()
	fx.esc.sweep()
	fx.newEscalator(escalationTestRecipient).sweep()

	if got := fx.mail.attempts(); got != 1 {
		t.Fatalf("send attempts = %d, want 1", got)
	}
	if got := fx.disposition(t); got != session.AlertDispositionSending {
		t.Fatalf("disposition = %q, want sending", got)
	}
	if !strings.Contains(fx.stderr.String(), "state write refused") {
		t.Fatalf("outcome write failure not logged: %q", fx.stderr.String())
	}
}

// The store's pending-to-sending compare-and-set is the one-shot guard, and it
// is not atomic across callers (SetStartupHealthAlert): the controller runs
// one sweep at a time, and each tick builds a fresh escalator. So a stale
// pending observation replayed by later ticks' escalators, each over the same
// durable store, claims the episode once.
func TestStartupHealthEscalationReplayedObservationClaimsOnce(t *testing.T) {
	fx := newEscalationFixture(t, escalationTestRecipient)
	ep := pendingEpisode("s1", session.FailureKindOther)
	fx.seed(t, ep)

	for i := 0; i < 8; i++ {
		fx.newEscalator(escalationTestRecipient).escalate(ep)
	}

	if got := fx.mail.attempts(); got != 1 {
		t.Fatalf("send attempts = %d, want 1", got)
	}
}

// Successive ticks' sweeps, each a fresh escalator over the same store, send
// one escalation for the episode.
func TestStartupHealthEscalationRepeatedSweepsSendOnce(t *testing.T) {
	fx := newEscalationFixture(t, escalationTestRecipient)
	fx.seed(t, pendingEpisode("s1", session.FailureKindOther))

	for i := 0; i < 8; i++ {
		fx.newEscalator(escalationTestRecipient).sweep()
	}

	if got := fx.mail.attempts(); got != 1 {
		t.Fatalf("send attempts = %d, want 1", got)
	}
}

// The escalator takes its provider from the running controller's class-routed
// state and never builds a mail provider of its own.
func TestNewStartupHealthEscalatorUsesControllerClassRoutedMailProvider(t *testing.T) {
	routed := mail.NewFake()
	cs := &controllerState{cityMailProv: routed}
	cfg := &config.City{Session: config.SessionConfig{EscalationTo: " " + escalationTestRecipient + " "}}
	store := beads.NewMemStore()
	var stderr bytes.Buffer

	esc := newStartupHealthEscalator(sessionFrontDoor(store), cs, cfg, t.TempDir(), &stderr)
	if err := esc.store.SaveStartupHealthEpisode(pendingEpisode("s1", session.FailureKindOther)); err != nil {
		t.Fatal(err)
	}
	esc.sweep()

	msgs := routed.Messages()
	if len(msgs) != 1 || msgs[0].To != escalationTestRecipient {
		t.Fatalf("routed provider messages = %+v, want one to %q", msgs, escalationTestRecipient)
	}
}

func TestNewStartupHealthEscalatorWithoutControllerStateHasNoProvider(t *testing.T) {
	cfg := &config.City{Session: config.SessionConfig{EscalationTo: escalationTestRecipient}}
	esc := newStartupHealthEscalator(sessionFrontDoor(beads.NewMemStore()), nil, cfg, t.TempDir(), &bytes.Buffer{})
	if esc.mail != nil {
		t.Fatal("a nil controller state must yield a nil provider, not a fresh one")
	}
}

func TestStartupHealthEscalationTailIsBoundedAndKeepsTheEnd(t *testing.T) {
	fx := newEscalationFixture(t, escalationTestRecipient)
	fx.seed(t, pendingEpisode("s1", session.FailureKindStartupDeath))
	fx.writeStderr(t, "FIRST-LINE-MARKER\n"+strings.Repeat("x", 100_000)+"\nLAST-LINE-MARKER\n")

	fx.esc.sweep()

	body := fx.onlyMessage(t).Body
	if !strings.Contains(body, "LAST-LINE-MARKER") {
		t.Error("body lost the end of start-stderr.log")
	}
	if strings.Contains(body, "FIRST-LINE-MARKER") {
		t.Error("body kept the head of start-stderr.log")
	}
	if len(body) > startupHealthEscalationTailBytes+2048 {
		t.Errorf("body is %d bytes, want it bounded near %d", len(body), startupHealthEscalationTailBytes)
	}
}

func TestReadStartupStderrTailNeverSplitsARune(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "start-stderr.log")
	// Two-byte runes: an odd limit always lands inside one.
	if err := os.WriteFile(path, []byte(strings.Repeat("é", 200)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{7, 8, 51, 100} {
		got, err := readStartupStderrTail(path, limit)
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if !utf8.ValidString(got) {
			t.Errorf("limit %d: tail is not valid UTF-8: %q", limit, got)
		}
		if len(got) > limit {
			t.Errorf("limit %d: tail is %d bytes", limit, len(got))
		}
		if strings.Trim(got, "é") != "" {
			t.Errorf("limit %d: tail %q is not whole runes", limit, got)
		}
	}
}

func TestReadStartupStderrTailShortFileIsReturnedWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "start-stderr.log")
	if err := os.WriteFile(path, []byte("boom\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readStartupStderrTail(path, 4096)
	if err != nil || got != "boom\n" {
		t.Fatalf("got (%q, %v), want (boom\\n, nil)", got, err)
	}
}

func TestStartupHealthEscalationRedactsSecretsInTail(t *testing.T) {
	t.Setenv("GC_TEST_ESCALATION_API_TOKEN", "sk-live-0123456789abcdef")
	fx := newEscalationFixture(t, escalationTestRecipient)
	fx.seed(t, pendingEpisode("s1", session.FailureKindStartupDeath))
	fx.writeStderr(t, "auth failed using sk-live-0123456789abcdef against api\n")

	fx.esc.sweep()

	body := fx.onlyMessage(t).Body
	if strings.Contains(body, "sk-live-0123456789abcdef") {
		t.Fatalf("secret leaked into mail body:\n%s", body)
	}
	if !strings.Contains(body, "auth failed") {
		t.Fatalf("redaction removed the surrounding context:\n%s", body)
	}
}

func TestStartupHealthEscalationMissingStderrLogStillSends(t *testing.T) {
	fx := newEscalationFixture(t, escalationTestRecipient)
	fx.seed(t, pendingEpisode("s1", session.FailureKindStartupDeath))

	fx.esc.sweep()

	body := fx.onlyMessage(t).Body
	if !strings.Contains(body, "start-stderr.log") {
		t.Fatalf("body does not say the log is unavailable:\n%s", body)
	}
	if got := fx.disposition(t); got != session.AlertDispositionSent {
		t.Fatalf("disposition = %q, want sent", got)
	}
}

func TestStartupHealthEscalationTailOnlyForStartupDeath(t *testing.T) {
	fx := newEscalationFixture(t, escalationTestRecipient)
	fx.seed(t, pendingEpisode("s1", session.FailureKindOther))
	fx.writeStderr(t, "SHOULD-NOT-APPEAR\n")

	fx.esc.sweep()

	if strings.Contains(fx.onlyMessage(t).Body, "SHOULD-NOT-APPEAR") {
		t.Fatal("a non-startup_death episode attached the stderr tail")
	}
}

func TestStartupHealthEscalationSessionNameCannotEscapeDiagnosticsDir(t *testing.T) {
	// The log is read by the runtime session name; a pool row's differs from
	// its episode key, so both names are held to the same guard.
	cases := map[string]func(session.StartupHealthEpisode) session.StartupHealthEpisode{
		"episode key": func(ep session.StartupHealthEpisode) session.StartupHealthEpisode {
			ep.SessionName = "../../../outside"
			return ep
		},
		"runtime name": func(ep session.StartupHealthEpisode) session.StartupHealthEpisode {
			ep.RuntimeSessionName = "../../../outside"
			return ep
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			fx := newEscalationFixture(t, escalationTestRecipient)
			outside := filepath.Join(fx.cityPath, "outside")
			if err := os.MkdirAll(outside, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(outside, "start-stderr.log"), []byte("ESCAPED\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			fx.seed(t, mutate(pendingEpisode("s1-pool", session.FailureKindStartupDeath)))

			fx.esc.sweep()

			if strings.Contains(fx.onlyMessage(t).Body, "ESCAPED") {
				t.Fatal("session name traversed out of the diagnostics directory")
			}
		})
	}
}

func TestStartupHealthEscalationPhaseIsRegisteredForBothReconcilers(t *testing.T) {
	for name, phases := range map[string][]tickPhase{
		"legacy": legacyTickPhases,
		"v2":     maintenancePhases(legacyTickPhases),
	} {
		found := false
		for _, ph := range phases {
			if ph.name == "escalate_startup_health" {
				found = true
				if ph.session {
					t.Errorf("%s: escalate_startup_health must not be a session phase", name)
				}
			}
		}
		if !found {
			t.Errorf("%s tick phases do not run escalate_startup_health", name)
		}
	}
}

func TestStartupHealthEscalationStatesAreVisibleInDoctor(t *testing.T) {
	for disp, want := range map[session.AlertDisposition]string{
		session.AlertDispositionSending: "alert=sending",
		session.AlertDispositionSent:    "alert=sent",
		session.AlertDispositionSkipped: "alert=skipped",
		session.AlertDispositionFailed:  "alert=failed",
	} {
		ep := pendingEpisode("s1", session.FailureKindOther)
		ep.AlertDisposition = disp
		if got := formatStartupHealthEpisodeDetail(ep); !strings.Contains(got, want) {
			t.Errorf("doctor detail %q missing %q", got, want)
		}
	}
}

// Roles are configuration. The escalator names no role; the recipient comes
// only from [session].escalation_to.
func TestStartupHealthEscalationNamesNoRole(t *testing.T) {
	src, err := os.ReadFile("startup_health_escalation.go")
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(src))
	for _, role := range []string{"mayor", "deacon", "polecat", "witness", "refinery", "overseer", "human"} {
		if strings.Contains(lower, role) {
			t.Errorf("startup_health_escalation.go mentions role %q", role)
		}
	}
}
