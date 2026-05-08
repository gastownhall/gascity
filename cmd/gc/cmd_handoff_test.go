package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func TestHandoffSuccess(t *testing.T) {
	store := beads.NewMemStore()
	rec := events.NewFake()
	dops := newFakeDrainOps()
	var stdout, stderr bytes.Buffer

	code := doHandoff(store, rec, dops, nil, "mayor", "mayor",
		[]string{"HANDOFF: context full"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}

	// Verify mail bead created.
	all, _ := store.ListOpen()
	if len(all) != 1 {
		t.Fatalf("got %d beads, want 1", len(all))
	}
	b := all[0]
	if b.Title != "HANDOFF: context full" {
		t.Errorf("Title = %q, want %q", b.Title, "HANDOFF: context full")
	}
	if b.Type != "message" {
		t.Errorf("Type = %q, want %q", b.Type, "message")
	}
	if b.Assignee != "mayor" {
		t.Errorf("Assignee = %q, want %q", b.Assignee, "mayor")
	}
	if b.From != "mayor" {
		t.Errorf("From = %q, want %q", b.From, "mayor")
	}
	if b.Description != "" {
		t.Errorf("Description = %q, want empty", b.Description)
	}

	// Verify restart-requested flag set.
	if !dops.restartRequested["mayor"] {
		t.Error("restart-requested flag not set")
	}

	// Verify events recorded.
	if len(rec.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(rec.Events))
	}
	if rec.Events[0].Type != events.MailSent {
		t.Errorf("event[0].Type = %q, want %q", rec.Events[0].Type, events.MailSent)
	}
	if rec.Events[1].Type != events.SessionDraining {
		t.Errorf("event[1].Type = %q, want %q", rec.Events[1].Type, events.SessionDraining)
	}
	if rec.Events[1].Message != "handoff" {
		t.Errorf("event[1].Message = %q, want %q", rec.Events[1].Message, "handoff")
	}

	// Verify stdout confirmation.
	if !strings.Contains(stdout.String(), "Handoff: sent mail") {
		t.Errorf("stdout = %q, want confirmation message", stdout.String())
	}
}

func TestCmdHandoffAutoSendsMailWithoutBlocking(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_ALIAS", "mayor")
	t.Setenv("GC_SESSION_NAME", "mayor")

	var stdout, stderr bytes.Buffer
	cmd := newHandoffCmd(&stdout, &stderr)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--auto", "context cycle"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("gc handoff --auto failed: %v; stderr=%s", err, stderr.String())
	}

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	all, err := store.ListOpen()
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d open beads, want 1", len(all))
	}
	if got := all[0].Title; got != "context cycle" {
		t.Fatalf("mail title = %q, want context cycle", got)
	}
	if got := all[0].Type; got != "message" {
		t.Fatalf("mail type = %q, want message", got)
	}
	if strings.Contains(stdout.String(), "requesting restart") {
		t.Fatalf("stdout = %q, --auto must not request restart", stdout.String())
	}
	if !strings.Contains(stdout.String(), "auto") {
		t.Fatalf("stdout = %q, want auto handoff confirmation", stdout.String())
	}
}

func TestCmdHandoffAutoUsesDefaultSubject(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_ALIAS", "mayor")
	t.Setenv("GC_SESSION_NAME", "mayor")

	var stdout, stderr bytes.Buffer
	cmd := newHandoffCmd(&stdout, &stderr)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--auto"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("gc handoff --auto failed: %v; stderr=%s", err, stderr.String())
	}

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	all, err := store.ListOpen()
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d open beads, want 1", len(all))
	}
	if got := all[0].Title; got != "context cycle" {
		t.Fatalf("mail title = %q, want context cycle", got)
	}
}

func TestCmdHandoffAutoRejectsTarget(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := cmdHandoff([]string{"context cycle"}, "mayor", true, &stdout, &stderr); code == 0 {
		t.Fatal("cmdHandoff returned 0 for --auto with --target")
	}
	if !strings.Contains(stderr.String(), "--auto cannot be used with --target") {
		t.Fatalf("stderr = %q, want --auto/--target conflict", stderr.String())
	}
}

// Regression for gastownhall/gascity#744:
// gc handoff on a named (human-attended) session used to call
// setRestartRequested unconditionally. The controller cannot respawn a
// user-started session, so the PreCompact hook crashed the user to their shell
// on every context compaction. doHandoff must recognize the named-session
// case, still send the handoff mail, and skip both the tmux and bead restart
// flags.
func TestDoHandoff_Regression744_NamedSessionSkipsRestart(t *testing.T) {
	store := beads.NewMemStore()
	rec := events.NewFake()
	dops := newFakeDrainOps()
	var stdout, stderr bytes.Buffer

	b, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{"gc:session"},
	})
	if err != nil {
		t.Fatalf("seeding session bead: %v", err)
	}
	if err := store.SetMetadata(b.ID, "session_name", "mayor"); err != nil {
		t.Fatalf("set session_name: %v", err)
	}
	if err := store.SetMetadata(b.ID, "configured_named_session", "true"); err != nil {
		t.Fatalf("set configured_named_session: %v", err)
	}
	if err := store.SetMetadata(b.ID, "configured_named_mode", "on_demand"); err != nil {
		t.Fatalf("set configured_named_mode: %v", err)
	}
	if err := store.SetMetadata(b.ID, "restart_requested", "true"); err != nil {
		t.Fatalf("set restart_requested: %v", err)
	}
	if err := store.SetMetadata(b.ID, "continuation_reset_pending", "true"); err != nil {
		t.Fatalf("set continuation_reset_pending: %v", err)
	}
	dops.restartRequested["mayor"] = true

	persistCalled := false
	outcome := doHandoffWithOutcome(store, rec, dops, func() error {
		persistCalled = true
		return nil
	}, "mayor", "mayor", []string{"HANDOFF: context full"}, &stdout, &stderr)
	if outcome.code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", outcome.code, stderr.String())
	}
	if outcome.restartRequested {
		t.Fatal("restartRequested = true, want false for on-demand named session")
	}

	mailFound := false
	all, _ := store.ListOpen()
	for _, got := range all {
		if got.Title == "HANDOFF: context full" && got.Type == "message" {
			mailFound = true
			break
		}
	}
	if !mailFound {
		t.Fatalf("handoff mail not created; beads=%v", all)
	}
	if dops.restartRequested["mayor"] {
		t.Errorf("restart-requested flag is still set; named sessions must skip restart")
	}
	if persistCalled {
		t.Error("persistRestart was called; named sessions must skip persisted restart requests")
	}
	refreshed, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("fetching seeded bead: %v", err)
	}
	if refreshed.Metadata["restart_requested"] != "" {
		t.Errorf("bead restart_requested = %q, want cleared for named session", refreshed.Metadata["restart_requested"])
	}
	if refreshed.Metadata["continuation_reset_pending"] != "" {
		t.Errorf("continuation_reset_pending = %q, want cleared for named session", refreshed.Metadata["continuation_reset_pending"])
	}
	if strings.Contains(stdout.String(), "requesting restart") {
		t.Errorf("stdout = %q, must not promise a restart for named sessions", stdout.String())
	}
	if len(rec.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(rec.Events))
	}
	if rec.Events[0].Type != events.MailSent {
		t.Fatalf("event[0].Type = %q, want %q", rec.Events[0].Type, events.MailSent)
	}
}

func TestDoHandoff_NamedSessionClearRestartFailureReturnsError(t *testing.T) {
	store := beads.NewMemStore()
	rec := events.NewFake()
	dops := newFakeDrainOps()
	dops.err = errors.New("tmux borked")
	var stdout, stderr bytes.Buffer

	b, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{"gc:session"},
	})
	if err != nil {
		t.Fatalf("seeding session bead: %v", err)
	}
	if err := store.SetMetadata(b.ID, "session_name", "mayor"); err != nil {
		t.Fatalf("set session_name: %v", err)
	}
	if err := store.SetMetadata(b.ID, "configured_named_session", "true"); err != nil {
		t.Fatalf("set configured_named_session: %v", err)
	}
	if err := store.SetMetadata(b.ID, "configured_named_mode", "on_demand"); err != nil {
		t.Fatalf("set configured_named_mode: %v", err)
	}

	outcome := doHandoffWithOutcome(store, rec, dops, nil, "mayor", "mayor",
		[]string{"HANDOFF: context full"}, &stdout, &stderr)
	if outcome.code != 1 {
		t.Fatalf("code = %d, want 1", outcome.code)
	}
	if outcome.restartRequested {
		t.Fatal("restartRequested = true, want false")
	}
	if !strings.Contains(stderr.String(), "clearing stale restart request") {
		t.Fatalf("stderr = %q, want stale restart cleanup error", stderr.String())
	}
	if strings.Contains(stdout.String(), "restart skipped") {
		t.Fatalf("stdout = %q, must not report success when cleanup fails", stdout.String())
	}
}

func TestDoHandoff_NamedAlwaysSessionRequestsRestart(t *testing.T) {
	store := beads.NewMemStore()
	rec := events.NewFake()
	dops := newFakeDrainOps()
	var stdout, stderr bytes.Buffer

	b, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{"gc:session"},
	})
	if err != nil {
		t.Fatalf("seeding session bead: %v", err)
	}
	if err := store.SetMetadata(b.ID, "session_name", "mayor"); err != nil {
		t.Fatalf("set session_name: %v", err)
	}
	if err := store.SetMetadata(b.ID, "configured_named_session", "true"); err != nil {
		t.Fatalf("set configured_named_session: %v", err)
	}
	if err := store.SetMetadata(b.ID, "configured_named_mode", "always"); err != nil {
		t.Fatalf("set configured_named_mode: %v", err)
	}

	persistCalled := false
	outcome := doHandoffWithOutcome(store, rec, dops, func() error {
		persistCalled = true
		return nil
	}, "mayor", "mayor", []string{"HANDOFF: context full"}, &stdout, &stderr)
	if outcome.code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", outcome.code, stderr.String())
	}
	if !outcome.restartRequested {
		t.Fatal("restartRequested = false, want true for always-mode named session")
	}
	if !dops.restartRequested["mayor"] {
		t.Error("restart-requested flag not set for always-mode named session")
	}
	if !persistCalled {
		t.Error("persistRestart was not called for always-mode named session")
	}
	if len(rec.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(rec.Events))
	}
	if rec.Events[1].Type != events.SessionDraining {
		t.Fatalf("event[1].Type = %q, want %q", rec.Events[1].Type, events.SessionDraining)
	}
}

func TestHandoffWithMessage(t *testing.T) {
	store := beads.NewMemStore()
	rec := events.NewFake()
	dops := newFakeDrainOps()
	var stdout, stderr bytes.Buffer

	code := doHandoff(store, rec, dops, nil, "polecat-1", "gc-city-polecat-1",
		[]string{"HANDOFF: PR review needed", "PR #42 is open, tests passing, needs review from refinery"},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}

	all, _ := store.ListOpen()
	if len(all) != 1 {
		t.Fatalf("got %d beads, want 1", len(all))
	}
	b := all[0]
	if b.Description != "PR #42 is open, tests passing, needs review from refinery" {
		t.Errorf("Description = %q, want body text", b.Description)
	}
}

func TestCmdHandoff_Regression744_NamedSessionReturnsWithoutBlocking(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_ALIAS", "mayor")
	t.Setenv("GC_SESSION_NAME", "mayor")

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	b, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{"gc:session"},
	})
	if err != nil {
		t.Fatalf("seeding session bead: %v", err)
	}
	if err := store.SetMetadata(b.ID, "session_name", "mayor"); err != nil {
		t.Fatalf("set session_name: %v", err)
	}
	if err := store.SetMetadata(b.ID, "configured_named_session", "true"); err != nil {
		t.Fatalf("set configured_named_session: %v", err)
	}
	if err := store.SetMetadata(b.ID, "configured_named_mode", "on_demand"); err != nil {
		t.Fatalf("set configured_named_mode: %v", err)
	}

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- cmdHandoff([]string{"HANDOFF: context full"}, "", false, &stdout, &stderr)
	}()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cmdHandoff blocked for named on-demand session")
	}
	if !strings.Contains(stdout.String(), "restart skipped") {
		t.Fatalf("stdout = %q, want restart skipped confirmation", stdout.String())
	}
}

func TestHandoffMissingSubject(t *testing.T) {
	store := beads.NewMemStore()
	rec := events.NewFake()
	dops := newFakeDrainOps()
	var stdout, stderr bytes.Buffer

	// Cobra enforces RangeArgs(1, 2), so doHandoff won't be called with 0 args.
	// Test at the cobra level.
	cmd := newHandoffCmd(&stdout, &stderr)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Error("handoff with no args should fail")
	}

	// Verify no side effects.
	all, _ := store.ListOpen()
	if len(all) != 0 {
		t.Errorf("got %d beads, want 0", len(all))
	}
	if len(rec.Events) != 0 {
		t.Errorf("got %d events, want 0", len(rec.Events))
	}
	if len(dops.restartRequested) != 0 {
		t.Error("restart-requested should not be set")
	}
}

func TestHandoffNotInSessionContext(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newHandoffCmd(&stdout, &stderr)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_CITY", "")
	cmd.SetArgs([]string{"HANDOFF: test"})
	err := cmd.Execute()
	if err == nil {
		t.Error("handoff without session context should fail")
	}
	if !strings.Contains(stderr.String(), "not in session context") {
		t.Errorf("stderr = %q, want 'not in session context' error", stderr.String())
	}
}

func TestHandoffRemoteRunning(t *testing.T) {
	store := beads.NewMemStore()
	rec := events.NewFake()
	sp := runtime.NewFake()
	// Start the target session.
	if err := sp.Start(context.Background(), "deacon", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := doHandoffRemote(store, rec, sp, "deacon", "deacon", "mayor",
		[]string{"Context refresh", "Check beads for current state"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}

	// Verify mail sent to target.
	all, _ := store.ListOpen()
	if len(all) != 1 {
		t.Fatalf("got %d beads, want 1", len(all))
	}
	b := all[0]
	if b.Assignee != "deacon" {
		t.Errorf("Assignee = %q, want %q", b.Assignee, "deacon")
	}
	if b.From != "mayor" {
		t.Errorf("From = %q, want %q", b.From, "mayor")
	}
	if b.Description != "Check beads for current state" {
		t.Errorf("Description = %q, want body text", b.Description)
	}

	// Verify session killed.
	if sp.IsRunning("deacon") {
		t.Error("target session should be stopped")
	}

	// Verify events: MailSent + SessionStopped.
	if len(rec.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(rec.Events))
	}
	if rec.Events[0].Type != events.MailSent {
		t.Errorf("event[0].Type = %q, want %q", rec.Events[0].Type, events.MailSent)
	}
	if rec.Events[1].Type != events.SessionStopped {
		t.Errorf("event[1].Type = %q, want %q", rec.Events[1].Type, events.SessionStopped)
	}

	// Verify stdout says killed.
	if !strings.Contains(stdout.String(), "killed session") {
		t.Errorf("stdout = %q, want 'killed session'", stdout.String())
	}
}

func TestHandoffRemoteNamedOnDemandSkipsKill(t *testing.T) {
	store := beads.NewMemStore()
	rec := events.NewFake()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "mayor", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	b, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{"gc:session"},
	})
	if err != nil {
		t.Fatalf("seeding session bead: %v", err)
	}
	if err := store.SetMetadata(b.ID, "session_name", "mayor"); err != nil {
		t.Fatalf("set session_name: %v", err)
	}
	if err := store.SetMetadata(b.ID, "configured_named_session", "true"); err != nil {
		t.Fatalf("set configured_named_session: %v", err)
	}
	if err := store.SetMetadata(b.ID, "configured_named_mode", "on_demand"); err != nil {
		t.Fatalf("set configured_named_mode: %v", err)
	}
	if err := store.SetMetadata(b.ID, "restart_requested", "true"); err != nil {
		t.Fatalf("set restart_requested: %v", err)
	}
	if err := store.SetMetadata(b.ID, "continuation_reset_pending", "true"); err != nil {
		t.Fatalf("set continuation_reset_pending: %v", err)
	}
	if err := sp.SetMeta("mayor", "GC_RESTART_REQUESTED", "1"); err != nil {
		t.Fatalf("set runtime restart meta: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := doHandoffRemote(store, rec, sp, "mayor", "mayor", "deacon",
		[]string{"Context refresh", "Please pick this up manually"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !sp.IsRunning("mayor") {
		t.Error("named on-demand target should still be running")
	}
	if len(rec.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(rec.Events))
	}
	if rec.Events[0].Type != events.MailSent {
		t.Fatalf("event[0].Type = %q, want %q", rec.Events[0].Type, events.MailSent)
	}
	if strings.Contains(stdout.String(), "killed session") {
		t.Errorf("stdout = %q, must not report killing a named on-demand session", stdout.String())
	}
	if !strings.Contains(stdout.String(), "named session") {
		t.Errorf("stdout = %q, want named-session skip confirmation", stdout.String())
	}
	refreshed, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("fetching seeded bead: %v", err)
	}
	if refreshed.Metadata["restart_requested"] != "" {
		t.Errorf("bead restart_requested = %q, want cleared for named target", refreshed.Metadata["restart_requested"])
	}
	if refreshed.Metadata["continuation_reset_pending"] != "" {
		t.Errorf("continuation_reset_pending = %q, want cleared for named target", refreshed.Metadata["continuation_reset_pending"])
	}
	if got, err := sp.GetMeta("mayor", "GC_RESTART_REQUESTED"); err != nil || got != "" {
		t.Errorf("runtime restart meta = %q, err=%v; want cleared", got, err)
	}
}

func TestHandoffRemoteNotRunning(t *testing.T) {
	store := beads.NewMemStore()
	rec := events.NewFake()
	sp := runtime.NewFake()
	var stdout, stderr bytes.Buffer
	code := doHandoffRemote(store, rec, sp, "deacon", "deacon", "human",
		[]string{"Please check on PR #42"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}

	// Mail still sent even if session not running.
	all, _ := store.ListOpen()
	if len(all) != 1 {
		t.Fatalf("got %d beads, want 1", len(all))
	}

	// Only MailSent event (no SessionStopped since not running).
	if len(rec.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(rec.Events))
	}

	// Stdout mentions not running.
	if !strings.Contains(stdout.String(), "not running") {
		t.Errorf("stdout = %q, want 'not running' mention", stdout.String())
	}
}

func TestCmdHandoffRemoteDefaultSenderFallsBackToGCAliasWhenSessionIDMissing(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_MAIL", "")

	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", cityPath)

	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	senderBead, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "sender",
			"session_name": "sender-gc-42",
		},
	})
	if err != nil {
		t.Fatalf("Create sender: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "recipient",
			"session_name": "recipient-gc-42",
		},
	}); err != nil {
		t.Fatalf("Create recipient: %v", err)
	}

	t.Setenv("GC_SESSION_ID", "gc-does-not-match")
	t.Setenv("GC_ALIAS", "sender")
	_ = os.Unsetenv("GC_AGENT")

	var stdout, stderr bytes.Buffer
	code := cmdHandoffRemote([]string{"Context refresh", "Check current state"}, "recipient", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdHandoffRemote() = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	storeAfter, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt after handoff: %v", err)
	}
	all, err := storeAfter.ListOpen()
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	var msg beads.Bead
	found := false
	for _, b := range all {
		if b.Type == "message" {
			msg = b
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("message bead not found; beads=%#v", all)
	}
	if msg.From != "sender" {
		t.Fatalf("message From = %q, want sender", msg.From)
	}
	if msg.Metadata["mail.from_session_id"] != senderBead.ID {
		t.Fatalf("mail.from_session_id = %q, want %q", msg.Metadata["mail.from_session_id"], senderBead.ID)
	}
	if msg.Metadata["mail.from_display"] != "sender" {
		t.Fatalf("mail.from_display = %q, want sender", msg.Metadata["mail.from_display"])
	}
	if msg.Assignee != "recipient" {
		t.Fatalf("message Assignee = %q, want recipient", msg.Assignee)
	}
}
