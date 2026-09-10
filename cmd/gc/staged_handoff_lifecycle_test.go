package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	redHandoffStagedLabel                  = "gc:handoff-staged"
	redHandoffStagedMetadataKey            = "mail.staged"
	redHandoffStagedForTokenKey            = "mail.staged_for_token"
	redHandoffStageCommittedAtKey          = "handoff_stage_committed_at"
	redHandoffStagedMessageIDKey           = "handoff_staged_message_id"
	redHandoffReleaseAttemptedAtKey        = "handoff_release_attempted_at"
	redSessionHandoffStagedEvent           = "session.handoff_staged"
	redSessionHandoffRestartAcceptedEvent  = "session.handoff_restart_accepted"
	redSessionHandoffSuccessorStartedEvent = "session.handoff_successor_started"
	redSessionHandoffReleasedEvent         = "session.handoff_released"
	redSessionHandoffFailedEvent           = "session.handoff_failed"
)

type stagedHandoffStoppedProvider struct {
	*runtime.Fake
}

func (p *stagedHandoffStoppedProvider) GetMeta(name, key string) (string, error) {
	value, err := p.Fake.GetMeta(name, key)
	if key == "GC_RESTART_REQUESTED" {
		return "", err
	}
	return value, err
}

func TestRestartableSelfHandoffStagesBeforeRestartRequest(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead := seedRestartableHandoffSession(t, store, "worker", "origin-instance")
	recorder := events.NewFake()
	dops := newFakeDrainOps()
	var stdout, stderr bytes.Buffer
	var stagedBeforeRestartPatch bool
	var inspectErr error

	outcome := doHandoffWithOutcome(store, store, recorder, dops, func() error {
		stagedBeforeRestartPatch, inspectErr = hasDurableStagedHandoff(store)
		return store.SetMetadataBatch(sessionBead.ID, map[string]string{
			"restart_requested":          "true",
			"continuation_reset_pending": "true",
		})
	}, "worker", "worker", []string{"context cycle", "resume from this brief"}, &stdout, &stderr)
	if outcome.code != 0 || !outcome.restartRequested {
		t.Fatalf("handoff outcome = %#v, want accepted restart; stdout=%q stderr=%q", outcome, stdout.String(), stderr.String())
	}
	if inspectErr != nil {
		t.Fatalf("inspect staged handoff before restart: %v", inspectErr)
	}
	if !stagedBeforeRestartPatch {
		t.Fatal("restart request became visible before the handoff brief was durably staged")
	}

	message := onlyOpenHandoffMessage(t, store)
	assertHandoffProvisionalRecord(t, message, "origin-instance")
	updated, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if updated.Metadata[redHandoffStageCommittedAtKey] == "" {
		t.Fatal("session has no durable handoff stage commit marker")
	}
	if updated.Metadata[redHandoffStagedMessageIDKey] != message.ID {
		t.Fatalf("handoff_staged_message_id = %q, want %q", updated.Metadata[redHandoffStagedMessageIDKey], message.ID)
	}

	provider := beadmail.New(store)
	if visible, err := provider.Check("worker"); err != nil || len(visible) != 0 {
		t.Fatalf("Check before successor = (%#v, %v), want no visible handoff", visible, err)
	}
	assertHandoffEventOrder(t, recorder.Events, redSessionHandoffStagedEvent, redSessionHandoffRestartAcceptedEvent)
	assertNoEventType(t, recorder.Events, events.MailSent)
	output := strings.ToLower(stdout.String())
	if !strings.Contains(output, "staged") || !strings.Contains(output, "restart") {
		t.Fatalf("stdout = %q, want durable staged/restart-accepted status", stdout.String())
	}
	if strings.Contains(output, "sent") || strings.Contains(output, "delivered") {
		t.Fatalf("stdout = %q, must not claim delivery before successor release", stdout.String())
	}
}

func TestFailedSelfHandoffRemainsProvisionalAndAuditable(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "restart rejected", err: errors.New("restart rejected")},
		{name: "restart canceled", err: context.Canceled},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := beads.NewMemStore()
			sessionBead := seedPinnedHandoffSession(t, store, "worker", "origin-instance")
			recorder := events.NewFake()
			dops := newFakeDrainOps()
			var stdout, stderr bytes.Buffer

			outcome := doHandoffWithOutcome(store, store, recorder, dops, func() error {
				return test.err
			}, "worker", "worker", []string{"context cycle", "resume from this brief"}, &stdout, &stderr)
			if outcome.code != 1 || outcome.restartRequested {
				t.Fatalf("handoff outcome = %#v, want failed restart", outcome)
			}
			if dops.restartRequested["worker"] {
				t.Fatal("runtime restart flag remained set after durable restart rejection")
			}
			message := onlyOpenHandoffMessage(t, store)
			assertHandoffProvisionalRecord(t, message, "origin-instance")
			updated, err := store.Get(sessionBead.ID)
			if err != nil {
				t.Fatalf("get session after failed restart: %v", err)
			}
			if updated.Metadata[redHandoffStageCommittedAtKey] == "" || updated.Metadata[redHandoffStagedMessageIDKey] != message.ID {
				t.Fatalf("failed restart lost its durable staged-hand-off audit: %#v", updated.Metadata)
			}
			provider := beadmail.New(store)
			if visible, checkErr := provider.Check("worker"); checkErr != nil || len(visible) != 0 {
				t.Fatalf("Check after restart failure = (%#v, %v), want provisional handoff hidden", visible, checkErr)
			}
			assertEventCount(t, recorder.Events, redSessionHandoffFailedEvent, 1)
			assertNoEventType(t, recorder.Events, events.MailSent)
			output := strings.ToLower(stdout.String())
			if strings.Contains(output, "sent") || strings.Contains(output, "delivered") {
				t.Fatalf("stdout = %q, must not claim delivery for failed restart", stdout.String())
			}
			if !strings.Contains(stderr.String(), test.err.Error()) {
				t.Fatalf("stderr = %q, want restart failure %q", stderr.String(), test.err)
			}
		})
	}
}

func TestControllerLossLeavesSelfHandoffProvisional(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_HOME", filepath.Join(t.TempDir(), "gc-home"))
	cityPath := shortSocketTempDir(t, "gc-staged-controller-loss-")
	writeCityTOML(t, cityPath, "test-city", "worker")
	writeBuiltinImportsFixture(t, cityPath, "core")
	t.Setenv("GC_CITY", cityPath)
	t.Setenv("GC_CITY_PATH", cityPath)
	t.Setenv("GC_CEILING_DIRECTORIES", filepath.Dir(cityPath))
	t.Setenv("GC_ALIAS", "worker")
	t.Setenv("GC_AGENT", "worker")
	t.Setenv("GC_SESSION_NAME", "test-city--worker")
	t.Setenv("GC_TMUX_SESSION", "test-city--worker")

	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	sessionBead := seedRestartableHandoffSession(t, store, "test-city--worker", "origin-instance")
	t.Setenv("GC_SESSION_ID", sessionBead.ID)
	baseProvider := runtime.NewFake()
	provider := &stagedHandoffStoppedProvider{Fake: baseProvider}
	oldBuild := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return provider, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })

	var stdout, stderr bytes.Buffer
	if code := cmdHandoff([]string{"context cycle", "resume from this brief"}, "", false, "", &stdout, &stderr); code != 1 {
		t.Fatalf("handoff exit = %d, want 1 when controller is unavailable; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	message := onlyOpenHandoffMessage(t, store)
	assertHandoffProvisionalRecord(t, message, "origin-instance")
	mailProvider := beadmail.New(store)
	if visible, err := mailProvider.Check("worker"); err != nil || len(visible) != 0 {
		t.Fatalf("Check after controller loss = (%#v, %v), want provisional handoff hidden", visible, err)
	}
	if got := strings.ToLower(stdout.String()); strings.Contains(got, "sent") || strings.Contains(got, "delivered") {
		t.Fatalf("stdout = %q, must not claim delivery after controller loss", stdout.String())
	}
	if got := strings.ToLower(stderr.String()); !strings.Contains(got, "controller") || !strings.Contains(got, "restart request remains") {
		t.Fatalf("stderr = %q, want truthful controller-loss and durable-pending diagnostic", stderr.String())
	}
}

func TestNonRestartingHandoffModesRemainOrdinaryMail(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, store beads.Store, stdout, stderr *bytes.Buffer) int
	}{
		{
			name: "auto",
			run: func(_ *testing.T, store beads.Store, stdout, stderr *bytes.Buffer) int {
				return doHandoffAuto(store, store, events.Discard, "worker", []string{"context cycle"}, "", stdout, stderr)
			},
		},
		{
			name: "remote target",
			run: func(_ *testing.T, store beads.Store, stdout, stderr *bytes.Buffer) int {
				return doHandoffRemote(store, store, events.Discard, runtime.NewFake(), "remote", "remote", "worker",
					[]string{"context cycle"}, stdout, stderr)
			},
		},
		{
			name: "on-demand named session",
			run: func(t *testing.T, store beads.Store, stdout, stderr *bytes.Buffer) int {
				sessionBead := seedRestartableHandoffSession(t, store, "worker", "origin-instance")
				if err := store.SetMetadataBatch(sessionBead.ID, map[string]string{
					namedSessionMetadataKey:  "true",
					namedSessionModeMetadata: "on_demand",
				}); err != nil {
					t.Fatalf("configure named session: %v", err)
				}
				return doHandoff(store, store, events.Discard, newFakeDrainOps(), nil, "worker", "worker",
					[]string{"context cycle"}, stdout, stderr)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := beads.NewMemStore()
			var stdout, stderr bytes.Buffer
			if code := test.run(t, store, &stdout, &stderr); code != 0 {
				t.Fatalf("handoff exit = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			message := onlyOpenHandoffMessage(t, store)
			if hasString(message.Labels, redHandoffStagedLabel) || message.Metadata[redHandoffStagedMetadataKey] != "" {
				t.Fatalf("non-restarting handoff was made provisional: labels=%#v metadata=%#v", message.Labels, message.Metadata)
			}
			provider := beadmail.New(store)
			visible, err := provider.Check(message.Assignee)
			if err != nil {
				t.Fatalf("Check ordinary handoff: %v", err)
			}
			if len(visible) != 1 || visible[0].ID != message.ID {
				t.Fatalf("Check = %#v, want ordinary handoff %q", visible, message.ID)
			}
		})
	}
}

func TestLiveSuccessorReleasesStagedHandoffExactlyOnce(t *testing.T) {
	env, sessionBead, message := newStagedSuccessorReconcileScenario(t, "origin-instance", "successor-instance")
	recorder := events.NewFake()
	env.rec = recorder

	env.reconcile([]beads.Bead{sessionBead})
	assertHandoffReleased(t, env.store, sessionBead.ID, message.ID)
	assertHandoffEventOrder(t, recorder.Events, redSessionHandoffSuccessorStartedEvent, redSessionHandoffReleasedEvent)
	assertEventCount(t, recorder.Events, redSessionHandoffSuccessorStartedEvent, 1)
	assertEventCount(t, recorder.Events, redSessionHandoffReleasedEvent, 1)

	provider := beadmail.New(env.store)
	visible, err := provider.CheckAutoHandoffs([]string{"worker"})
	if err != nil {
		t.Fatalf("CheckAutoHandoffs after successor: %v", err)
	}
	if len(visible) != 1 || visible[0].ID != message.ID {
		t.Fatalf("CheckAutoHandoffs = %#v, want released handoff %q", visible, message.ID)
	}
	if err := provider.ArchiveInjectedAutoHandoffs([]string{message.ID}); err != nil {
		t.Fatalf("ArchiveInjectedAutoHandoffs: %v", err)
	}

	// A fresh provider models the successor process starting again; a second
	// reconcile models retry/replay of the controller observation.
	provider = beadmail.New(env.store)
	refreshedSession, err := env.store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("get session before replay: %v", err)
	}
	env.reconcile([]beads.Bead{refreshedSession})
	again, err := provider.CheckAutoHandoffs([]string{"worker"})
	if err != nil {
		t.Fatalf("CheckAutoHandoffs after replay: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("handoff delivered again after reconcile/process replay: %#v", again)
	}
	assertEventCount(t, recorder.Events, redSessionHandoffSuccessorStartedEvent, 1)
	assertEventCount(t, recorder.Events, redSessionHandoffReleasedEvent, 1)
}

func TestOriginatingIncarnationCannotReleaseStagedHandoff(t *testing.T) {
	env, sessionBead, message := newStagedSuccessorReconcileScenario(t, "same-instance", "same-instance")
	recorder := events.NewFake()
	env.rec = recorder

	env.reconcile([]beads.Bead{sessionBead})
	staged, err := env.store.Get(message.ID)
	if err != nil {
		t.Fatalf("get staged handoff: %v", err)
	}
	assertHandoffProvisionalRecord(t, staged, "same-instance")
	provider := beadmail.New(env.store)
	if visible, err := provider.CheckAutoHandoffs([]string{"worker"}); err != nil || len(visible) != 0 {
		t.Fatalf("originating incarnation saw its own handoff = (%#v, %v)", visible, err)
	}
	assertEventCount(t, recorder.Events, redSessionHandoffSuccessorStartedEvent, 0)
	assertEventCount(t, recorder.Events, redSessionHandoffReleasedEvent, 0)
}

func TestStagedHandoffTimeoutNeverReleasesToOriginatingIncarnation(t *testing.T) {
	env, sessionBead, message := newStagedSuccessorReconcileScenario(t, "same-instance", "same-instance")
	env.setSessionMetadata(&sessionBead, map[string]string{
		redHandoffStageCommittedAtKey: env.clk.Now().Add(-controllerRestartTimeout(env.cfg) - time.Second).Format(time.RFC3339),
	})
	recorder := events.NewFake()
	env.rec = recorder

	env.reconcile([]beads.Bead{sessionBead})
	refreshedSession, err := env.store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("get session after timeout: %v", err)
	}
	env.reconcile([]beads.Bead{refreshedSession})

	staged, err := env.store.Get(message.ID)
	if err != nil {
		t.Fatalf("get staged handoff: %v", err)
	}
	assertHandoffProvisionalRecord(t, staged, "same-instance")
	provider := beadmail.New(env.store)
	if visible, err := provider.CheckAutoHandoffs([]string{"worker"}); err != nil || len(visible) != 0 {
		t.Fatalf("CheckAutoHandoffs after timeout = (%#v, %v), want no exposed handoff", visible, err)
	}
	assertEventCount(t, recorder.Events, redSessionHandoffFailedEvent, 1)
	assertEventCount(t, recorder.Events, redSessionHandoffReleasedEvent, 0)
	if refreshedSession.Metadata[redHandoffReleaseAttemptedAtKey] == "" {
		t.Fatal("timed-out staged handoff has no durable release-attempt audit marker")
	}
}

func seedRestartableHandoffSession(t *testing.T, store beads.Store, name, instanceToken string) beads.Bead {
	t.Helper()
	created, err := store.Create(beads.Bead{
		Title:  name,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"alias":          name,
			"agent_name":     name,
			"template":       name,
			"session_name":   name,
			"state":          "active",
			"instance_token": instanceToken,
		},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return created
}

func seedPinnedHandoffSession(t *testing.T, store beads.Store, name, instanceToken string) beads.Bead {
	t.Helper()
	created := seedRestartableHandoffSession(t, store, name, instanceToken)
	if err := store.SetMetadataBatch(created.ID, map[string]string{
		namedSessionMetadataKey:  "true",
		namedSessionModeMetadata: "always",
		"pin_awake":              "true",
	}); err != nil {
		t.Fatalf("pin session: %v", err)
	}
	return created
}

func onlyOpenHandoffMessage(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	messages := listOpenMessagesBothTiers(t, store)
	if len(messages) != 1 {
		t.Fatalf("open handoff messages = %#v, want exactly one", messages)
	}
	return messages[0]
}

func hasDurableStagedHandoff(store beads.Store) (bool, error) {
	messages, err := store.List(beads.ListQuery{
		Status:    "open",
		Type:      "message",
		TierMode:  beads.TierBoth,
		AllowScan: true,
	})
	if err != nil || len(messages) != 1 {
		return false, err
	}
	message := messages[0]
	return hasString(message.Labels, redHandoffStagedLabel) &&
		message.Metadata[redHandoffStagedMetadataKey] == "true" &&
		message.Metadata[redHandoffStagedForTokenKey] != "", nil
}

func assertHandoffProvisionalRecord(t *testing.T, message beads.Bead, instanceToken string) {
	t.Helper()
	if !hasString(message.Labels, redHandoffStagedLabel) {
		t.Fatalf("handoff labels = %#v, missing %q", message.Labels, redHandoffStagedLabel)
	}
	if message.Metadata[redHandoffStagedMetadataKey] != "true" {
		t.Fatalf("mail.staged = %q, want true", message.Metadata[redHandoffStagedMetadataKey])
	}
	if message.Metadata[redHandoffStagedForTokenKey] != instanceToken {
		t.Fatalf("mail.staged_for_token = %q, want %q", message.Metadata[redHandoffStagedForTokenKey], instanceToken)
	}
}

func newStagedSuccessorReconcileScenario(t *testing.T, originToken, currentToken string) (*reconcilerTestEnv, beads.Bead, mail.Message) {
	t.Helper()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "test-cmd"}},
	}
	env.addDesired("worker", "worker", true)
	sessionBead := env.createSessionBead("worker", "worker")
	env.markSessionActive(&sessionBead)
	env.setSessionMetadata(&sessionBead, map[string]string{
		"instance_token":                currentToken,
		"started_config_hash":           runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
		redHandoffStageCommittedAtKey:   env.clk.Now().Format(time.RFC3339),
		redHandoffReleaseAttemptedAtKey: "",
	})
	if err := env.sp.SetMeta("worker", "GC_SESSION_ID", sessionBead.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	if err := env.sp.SetMeta("worker", "GC_INSTANCE_TOKEN", currentToken); err != nil {
		t.Fatalf("SetMeta(GC_INSTANCE_TOKEN): %v", err)
	}

	provider := beadmail.New(env.store)
	message, err := provider.SendHandoff(mail.HandoffIntent{
		From:     "worker",
		To:       "worker",
		Subject:  "context cycle",
		Body:     "resume exactly once",
		ThreadID: "successor-release",
		ExtraLabels: []string{
			mail.AutoHandoffLabel,
			mail.ArchiveAfterInjectLabel,
			redHandoffStagedLabel,
		},
	})
	if err != nil {
		t.Fatalf("SendHandoff: %v", err)
	}
	if err := env.store.SetMetadataBatch(message.ID, map[string]string{
		redHandoffStagedMetadataKey: "true",
		redHandoffStagedForTokenKey: originToken,
	}); err != nil {
		t.Fatalf("stage handoff: %v", err)
	}
	env.setSessionMetadata(&sessionBead, map[string]string{
		redHandoffStagedMessageIDKey: message.ID,
	})
	return env, sessionBead, message
}

func assertHandoffReleased(t *testing.T, store beads.Store, sessionID, messageID string) {
	t.Helper()
	message, err := store.Get(messageID)
	if err != nil {
		t.Fatalf("get released handoff: %v", err)
	}
	if hasString(message.Labels, redHandoffStagedLabel) {
		t.Fatalf("released handoff still has %q: %#v", redHandoffStagedLabel, message.Labels)
	}
	if message.Metadata[redHandoffStagedMetadataKey] != "false" {
		t.Fatalf("released mail.staged = %q, want false", message.Metadata[redHandoffStagedMetadataKey])
	}
	sessionBead, err := store.Get(sessionID)
	if err != nil {
		t.Fatalf("get session after release: %v", err)
	}
	if sessionBead.Metadata[redHandoffStagedMessageIDKey] != "" || sessionBead.Metadata[redHandoffStageCommittedAtKey] != "" {
		t.Fatalf("session handoff markers not cleared after release: %#v", sessionBead.Metadata)
	}
}

func assertHandoffEventOrder(t *testing.T, got []events.Event, first, second string) {
	t.Helper()
	firstIndex, secondIndex := -1, -1
	for index, event := range got {
		switch event.Type {
		case first:
			if firstIndex == -1 {
				firstIndex = index
			}
		case second:
			if secondIndex == -1 {
				secondIndex = index
			}
		}
	}
	if firstIndex == -1 || secondIndex == -1 || firstIndex >= secondIndex {
		t.Fatalf("event order = %#v, want %q before %q", got, first, second)
	}
}

func assertEventCount(t *testing.T, got []events.Event, eventType string, want int) {
	t.Helper()
	count := 0
	for _, event := range got {
		if event.Type == eventType {
			count++
		}
	}
	if count != want {
		t.Fatalf("event %q count = %d, want %d; events=%#v", eventType, count, want, got)
	}
}

func assertNoEventType(t *testing.T, got []events.Event, eventType string) {
	t.Helper()
	assertEventCount(t, got, eventType, 0)
}

var _ runtime.Provider = (*stagedHandoffStoppedProvider)(nil)
