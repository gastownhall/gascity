package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestHandoffSuccess(t *testing.T) {
	store := beads.NewMemStore()
	rec := events.NewFake()
	dops := newFakeDrainOps()
	var stdout, stderr bytes.Buffer

	code := doHandoff(store, rec, dops, "mayor", "mayor",
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

func TestHandoffWithMessage(t *testing.T) {
	store := beads.NewMemStore()
	rec := events.NewFake()
	dops := newFakeDrainOps()
	var stdout, stderr bytes.Buffer

	code := doHandoff(store, rec, dops, "polecat-1", "gc-city-polecat-1",
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
