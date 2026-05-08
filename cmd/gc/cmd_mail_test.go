package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	mailexec "github.com/gastownhall/gascity/internal/mail/exec"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/session"
)

type countOnlyMailProvider struct{}

type failingListByLabelStore struct {
	beads.Store
	err error
}

func (countOnlyMailProvider) Send(string, string, string, string) (mail.Message, error) {
	panic("unexpected Send")
}
func (countOnlyMailProvider) Inbox(string) ([]mail.Message, error) { panic("unexpected Inbox") }
func (countOnlyMailProvider) Get(string) (mail.Message, error)     { panic("unexpected Get") }
func (countOnlyMailProvider) Read(string) (mail.Message, error)    { panic("unexpected Read") }
func (countOnlyMailProvider) MarkRead(string) error                { panic("unexpected MarkRead") }
func (countOnlyMailProvider) MarkUnread(string) error              { panic("unexpected MarkUnread") }
func (countOnlyMailProvider) Archive(string) error                 { panic("unexpected Archive") }
func (countOnlyMailProvider) ArchiveMany([]string) ([]mail.ArchiveResult, error) {
	panic("unexpected ArchiveMany")
}
func (countOnlyMailProvider) Delete(string) error { panic("unexpected Delete") }
func (countOnlyMailProvider) DeleteMany([]string) ([]mail.ArchiveResult, error) {
	panic("unexpected DeleteMany")
}
func (countOnlyMailProvider) Check(string) ([]mail.Message, error) { panic("unexpected Check") }
func (countOnlyMailProvider) Reply(string, string, string, string) (mail.Message, error) {
	panic("unexpected Reply")
}
func (countOnlyMailProvider) Thread(string) ([]mail.Message, error) { panic("unexpected Thread") }
func (countOnlyMailProvider) All(string) ([]mail.Message, error)    { panic("unexpected All") }
func (countOnlyMailProvider) Count(recipient string) (int, int, error) {
	switch recipient {
	case "sky":
		return 2, 1, nil
	case "gc-1":
		return 1, 1, nil
	default:
		return 0, 0, nil
	}
}

func (s failingListByLabelStore) ListByLabel(_ string, _ int, _ ...beads.QueryOpt) ([]beads.Bead, error) {
	return nil, s.err
}

func (s failingListByLabelStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, s.err
}

// --- gc mail send ---

func TestMailSendSuccess(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	recipients := map[string]bool{"human": true, "mayor": true}

	var stdout, stderr bytes.Buffer
	code := doMailSend(mp, events.Discard, recipients, "human", []string{"mayor", "hey, are you still there?"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailSend = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Sent message gc-1 to mayor") {
		t.Errorf("stdout = %q, want sent confirmation", stdout.String())
	}

	// Verify the bead was created correctly.
	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Type != "message" {
		t.Errorf("bead Type = %q, want %q", b.Type, "message")
	}
	if b.Assignee != "mayor" {
		t.Errorf("bead Assignee = %q, want %q", b.Assignee, "mayor")
	}
	if b.From != "human" {
		t.Errorf("bead From = %q, want %q", b.From, "human")
	}
	// Body is now in Description (subject is empty for positional args).
	if b.Description != "hey, are you still there?" {
		t.Errorf("bead Description = %q, want message body", b.Description)
	}
	if b.Status != "open" {
		t.Errorf("bead Status = %q, want %q", b.Status, "open")
	}
}

func TestMailSendMissingArgs(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	recipients := map[string]bool{"human": true}

	tests := []struct {
		name string
		args []string
	}{
		{"no args", nil},
		{"only recipient", []string{"mayor"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := doMailSend(mp, events.Discard, recipients, "human", tt.args, nil, &bytes.Buffer{}, &stderr)
			if code != 1 {
				t.Errorf("doMailSend = %d, want 1", code)
			}
			if !strings.Contains(stderr.String(), "usage:") {
				t.Errorf("stderr = %q, want usage message", stderr.String())
			}
		})
	}
}

func TestMailSendInvalidRecipient(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	recipients := map[string]bool{"human": true, "mayor": true}

	var stderr bytes.Buffer
	code := doMailSend(mp, events.Discard, recipients, "human", []string{"nobody", "hello"}, nil, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doMailSend = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), `unknown recipient "nobody"`) {
		t.Errorf("stderr = %q, want unknown recipient error", stderr.String())
	}
}

func TestMailSendToHuman(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	recipients := map[string]bool{"human": true, "mayor": true}

	var stdout bytes.Buffer
	code := doMailSend(mp, events.Discard, recipients, "mayor", []string{"human", "task complete"}, nil, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("doMailSend = %d, want 0", code)
	}

	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Assignee != "human" {
		t.Errorf("bead Assignee = %q, want %q", b.Assignee, "human")
	}
	if b.From != "mayor" {
		t.Errorf("bead From = %q, want %q", b.From, "mayor")
	}
}

func TestMailSendAgentToAgent(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	recipients := map[string]bool{"human": true, "mayor": true, "worker": true}

	var stdout bytes.Buffer
	code := doMailSend(mp, events.Discard, recipients, "worker", []string{"mayor", "found a bug"}, nil, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("doMailSend = %d, want 0", code)
	}

	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.From != "worker" {
		t.Errorf("bead From = %q, want %q", b.From, "worker")
	}
	if b.Assignee != "mayor" {
		t.Errorf("bead Assignee = %q, want %q", b.Assignee, "mayor")
	}
}

func TestDefaultMailIdentityPrefersSessionIDOverGCAgentFallback(t *testing.T) {
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "mayor")
	t.Setenv("GC_SESSION_ID", "gc-123")

	if got := defaultMailIdentity(); got != "gc-123" {
		t.Fatalf("defaultMailIdentity() = %q, want gc-123", got)
	}
}

func TestDefaultMailIdentityCandidates_OrdersSessionIDFirstThenAliasThenAgent(t *testing.T) {
	t.Setenv("GC_ALIAS", "codeprobe-worker-1")
	t.Setenv("GC_SESSION_ID", "codeprobe-worker-gc-1941")
	t.Setenv("GC_AGENT", "codeprobe-worker")

	got := defaultMailIdentityCandidates()
	want := []string{"codeprobe-worker-gc-1941", "codeprobe-worker-1", "codeprobe-worker"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("defaultMailIdentityCandidates() = %#v, want %#v", got, want)
	}
}

func TestDefaultMailIdentityPrefersSessionIDOverAlias(t *testing.T) {
	t.Setenv("GC_ALIAS", "public-alias")
	t.Setenv("GC_AGENT", "worker")
	t.Setenv("GC_SESSION_ID", "session-123")

	if got := defaultMailIdentity(); got != "session-123" {
		t.Fatalf("defaultMailIdentity() = %q, want session-123", got)
	}
}

func TestDefaultMailIdentityCandidates_DedupesAndSkipsEmpty(t *testing.T) {
	t.Setenv("GC_ALIAS", "mayor")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_AGENT", "mayor")

	got := defaultMailIdentityCandidates()
	want := []string{"mayor"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("defaultMailIdentityCandidates() = %#v, want %#v", got, want)
	}
}

func TestDefaultMailIdentityCandidates_FallsBackToHumanWhenAllEmpty(t *testing.T) {
	t.Setenv("GC_ALIAS", "")
	_ = os.Unsetenv("GC_SESSION_ID")
	_ = os.Unsetenv("GC_AGENT")

	got := defaultMailIdentityCandidates()
	want := []string{"human"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("defaultMailIdentityCandidates() = %#v, want %#v", got, want)
	}
}

// TestResolveDefaultMailTargetsForCommand_UsesGCSessionIDBeforeAlias
// sets up two possible matches: GC_SESSION_ID points at the concrete worker,
// while GC_ALIAS points at another session. Default mail resolution must choose
// the concrete session identity first.
func TestResolveDefaultMailTargetsForCommand_UsesGCSessionIDBeforeAlias(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
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
	b, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "",
			"session_name": "codeprobe-worker-gc-1941",
		},
	})
	if err != nil {
		t.Fatalf("Create concrete session: %v", err)
	}
	aliasOnly, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "codeprobe-worker-1",
			"session_name": "stale-alias-session",
		},
	})
	if err != nil {
		t.Fatalf("Create alias session: %v", err)
	}

	t.Setenv("GC_ALIAS", "codeprobe-worker-1")
	t.Setenv("GC_SESSION_ID", "codeprobe-worker-gc-1941")
	t.Setenv("GC_AGENT", "codeprobe-worker")

	var stderr bytes.Buffer
	target, ok := resolveDefaultMailTargetsForCommand(&stderr, "gc mail inbox")
	if !ok {
		t.Fatalf("resolveDefaultMailTargetsForCommand() = not ok; stderr=%q", stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
	foundConcrete := false
	foundAliasOnly := false
	for _, r := range target.recipients {
		if r == b.ID {
			foundConcrete = true
		}
		if r == aliasOnly.ID {
			foundAliasOnly = true
		}
	}
	if !foundConcrete || foundAliasOnly {
		t.Fatalf("target.recipients = %#v, want concrete bead %q and not alias bead %q", target.recipients, b.ID, aliasOnly.ID)
	}
}

func TestResolveDefaultMailTargetsForCommand_FallsBackToGCAliasWhenSessionIDMissing(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
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
	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "sky",
			"session_name": "sky-gc-42",
		},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	t.Setenv("GC_ALIAS", "sky")
	t.Setenv("GC_SESSION_ID", "gc-does-not-match")
	_ = os.Unsetenv("GC_AGENT")

	var stderr bytes.Buffer
	target, ok := resolveDefaultMailTargetsForCommand(&stderr, "gc mail inbox")
	if !ok {
		t.Fatalf("resolveDefaultMailTargetsForCommand() = not ok; stderr=%q", stderr.String())
	}
	if target.display != "sky" {
		t.Fatalf("target.display = %q, want sky", target.display)
	}
}

func TestResolveDefaultMailSenderForCommand_UsesDisplayAliasBeforeSessionName(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
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
	b, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "gascity/workflows.codex-min-1",
			"session_name": "workflows__codex-min-mc-abc123",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cfg, _ := loadCityConfig(cityPath)

	t.Setenv("GC_SESSION_ID", b.ID)
	t.Setenv("GC_ALIAS", "gascity/workflows.codex-min-1")
	t.Setenv("GC_AGENT", "gascity/workflows.codex-min-1")

	var stderr bytes.Buffer
	sender, ok := resolveDefaultMailSenderForCommand(cityPath, cfg, store, &stderr, "gc mail send")
	if !ok {
		t.Fatalf("resolveDefaultMailSenderForCommand() = not ok; stderr=%q", stderr.String())
	}
	if sender != "gascity/workflows.codex-min-1" {
		t.Fatalf("sender = %q, want display alias", sender)
	}
}

func TestResolveMailIdentityWithConfig_ExplicitAliasUsesDisplayAlias(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
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
	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "gascity/workflows.codex-min-16",
			"session_name": "workflows__codex-min-mc-explicit",
		},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cfg, _ := loadCityConfig(cityPath)

	for _, from := range []string{"gascity/workflows.codex-min-16", "workflows.codex-min-16"} {
		t.Run(from, func(t *testing.T) {
			sender, err := resolveMailIdentityWithConfig(cityPath, cfg, store, from)
			if err != nil {
				t.Fatalf("resolveMailIdentityWithConfig(%q): %v", from, err)
			}
			if sender != "gascity/workflows.codex-min-16" {
				t.Fatalf("sender = %q, want display alias", sender)
			}
		})
	}
}

func TestResolveDefaultMailSenderForCommand_FallsBackToGCAliasWhenSessionIDMissing(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
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
	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "sky",
			"session_name": "sky-gc-42",
		},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cfg, _ := loadCityConfig(cityPath)

	t.Setenv("GC_SESSION_ID", "gc-does-not-match")
	t.Setenv("GC_ALIAS", "sky")
	_ = os.Unsetenv("GC_AGENT")

	var stderr bytes.Buffer
	sender, ok := resolveDefaultMailSenderForCommand(cityPath, cfg, store, &stderr, "gc mail send")
	if !ok {
		t.Fatalf("resolveDefaultMailSenderForCommand() = not ok; stderr=%q", stderr.String())
	}
	if sender != "sky" {
		t.Fatalf("sender = %q, want sky", sender)
	}
}

func TestCmdMailSendDefaultSenderFallsBackToGCAliasWhenSessionIDMissing(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
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
	code := cmdMailSend([]string{"recipient", "hello"}, false, false, "", "", "", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdMailSend() = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	storeAfter, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt after send: %v", err)
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

func TestResolveDefaultMailTargetsForCommand_HumanDefaultWhenNoEnv(t *testing.T) {
	t.Setenv("GC_MAIL", "fake")
	_ = os.Unsetenv("GC_ALIAS")
	_ = os.Unsetenv("GC_SESSION_ID")
	_ = os.Unsetenv("GC_AGENT")

	var stderr bytes.Buffer
	target, ok := resolveDefaultMailTargetsForCommand(&stderr, "gc mail inbox")
	if !ok {
		t.Fatalf("resolveDefaultMailTargetsForCommand() = not ok; stderr=%q", stderr.String())
	}
	if target.display != "human" {
		t.Fatalf("target.display = %q, want human", target.display)
	}
}

// TestResolveDefaultMailTargetsForCommand_StorelessProviderUsesFirstCandidate
// confirms the storeless-provider shortcut forwards only candidates[0] —
// the same identity defaultMailIdentity() returns — rather than iterating.
func TestResolveDefaultMailTargetsForCommand_StorelessProviderUsesFirstCandidate(t *testing.T) {
	t.Setenv("GC_MAIL", "fake")
	t.Setenv("GC_ALIAS", "codeprobe-worker-1")
	t.Setenv("GC_SESSION_ID", "codeprobe-worker-gc-1941")
	t.Setenv("GC_AGENT", "codeprobe-worker")
	prev := openMailTargetStore
	openMailTargetStore = func() (beads.Store, error) {
		return nil, fmt.Errorf("not in a city directory")
	}
	t.Cleanup(func() { openMailTargetStore = prev })

	var stderr bytes.Buffer
	target, ok := resolveDefaultMailTargetsForCommand(&stderr, "gc mail inbox")
	if !ok {
		t.Fatalf("resolveDefaultMailTargetsForCommand() = not ok; stderr=%q", stderr.String())
	}
	if target.display != "codeprobe-worker-gc-1941" {
		t.Fatalf("target.display = %q, want codeprobe-worker-gc-1941", target.display)
	}
	if len(target.recipients) != 1 || target.recipients[0] != "codeprobe-worker-gc-1941" {
		t.Fatalf("target.recipients = %#v, want [codeprobe-worker-gc-1941]", target.recipients)
	}
}

// TestResolveDefaultMailTargetsForCommand_SurfacesAmbiguousError_AndStops
// confirms that when a candidate produces a non-ErrSessionNotFound error
// (here: ErrAmbiguous from two beads sharing the same session_name), the
// loop surfaces it to stderr and stops iterating rather than falling
// through to the next candidate.
func TestResolveDefaultMailTargetsForCommand_SurfacesAmbiguousError_AndStops(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
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
	for i := 0; i < 2; i++ {
		if _, err := store.Create(beads.Bead{
			Type:     session.BeadType,
			Labels:   []string{session.LabelSession},
			Metadata: map[string]string{"session_name": "ambiguous-target"},
		}); err != nil {
			t.Fatalf("Create(%d): %v", i, err)
		}
	}

	t.Setenv("GC_SESSION_ID", "ambiguous-target")
	t.Setenv("GC_ALIAS", "would-resolve-if-reached")
	_ = os.Unsetenv("GC_AGENT")

	var stderr bytes.Buffer
	_, ok := resolveDefaultMailTargetsForCommand(&stderr, "gc mail inbox")
	if ok {
		t.Fatalf("resolveDefaultMailTargetsForCommand() ok = true, want false")
	}
	if !strings.Contains(stderr.String(), "ambiguous") {
		t.Fatalf("stderr = %q, want to contain ambiguous", stderr.String())
	}
}

func TestDefaultMailIdentityFallsBackToGCAgentWithoutAliasOrSession(t *testing.T) {
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "mayor")
	_ = os.Unsetenv("GC_SESSION_ID")

	if got := defaultMailIdentity(); got != "mayor" {
		t.Fatalf("defaultMailIdentity() = %q, want mayor", got)
	}
}

func TestDefaultMailIdentityFallsBackToHumanWithoutAliasSessionOrAgent(t *testing.T) {
	t.Setenv("GC_ALIAS", "")
	_ = os.Unsetenv("GC_AGENT")
	_ = os.Unsetenv("GC_SESSION_ID")

	if got := defaultMailIdentity(); got != "human" {
		t.Fatalf("defaultMailIdentity() = %q, want human", got)
	}
}

func TestResolveMailAddressForCommand_AllowsStorelessMailProvider(t *testing.T) {
	t.Setenv("GC_MAIL", "fake")
	prev := openMailTargetStore
	openMailTargetStore = func() (beads.Store, error) {
		return nil, fmt.Errorf("not in a city directory")
	}
	t.Cleanup(func() { openMailTargetStore = prev })

	var stderr bytes.Buffer
	address, ok := resolveMailAddressForCommand("robot", &stderr, "gc mail inbox")
	if !ok {
		t.Fatal("resolveMailAddressForCommand() = not ok, want ok")
	}
	if address != "robot" {
		t.Fatalf("address = %q, want robot", address)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestResolveMailTargetsIncludesAliasHistoryAndSessionID(t *testing.T) {
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "sky",
			"alias_history": "mayor,witness",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	target, err := resolveMailTargets(store, "sky")
	if err != nil {
		t.Fatalf("resolveMailTargets: %v", err)
	}
	if target.display != "sky" {
		t.Fatalf("display = %q, want sky", target.display)
	}
	want := []string{"sky", b.ID, "mayor", "witness"}
	if strings.Join(target.recipients, ",") != strings.Join(want, ",") {
		t.Fatalf("recipients = %#v, want %#v", target.recipients, want)
	}
}

func TestResolveMailTargets_BareRigScopedNamedUsesUniqueLiveConfiguredNamedSession(t *testing.T) {
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":                     "frontend/rig-worker",
			"alias_history":             "old-frontend-worker",
			"session_name":              "frontend--rig-worker",
			"configured_named_session":  "true",
			"configured_named_identity": "frontend/rig-worker",
			"configured_named_mode":     "always",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	target, err := resolveMailTargets(store, "rig-worker")
	if err != nil {
		t.Fatalf("resolveMailTargets: %v", err)
	}
	if target.display != "frontend/rig-worker" {
		t.Fatalf("display = %q, want frontend/rig-worker", target.display)
	}
	want := []string{"frontend/rig-worker", b.ID, "old-frontend-worker"}
	if strings.Join(target.recipients, ",") != strings.Join(want, ",") {
		t.Fatalf("recipients = %#v, want %#v", target.recipients, want)
	}
}

func TestResolveMailTargetsForCommand_FakeProviderDoesNotResolveHistoricalAlias(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_MAIL", "fake")

	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", cityPath)

	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "sky",
			"alias_history": "mayor",
		},
	}); err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	var stderr bytes.Buffer
	target, ok := resolveMailTargetsForCommand("mayor", &stderr, "gc mail inbox")
	if !ok {
		t.Fatal("resolveMailTargetsForCommand() = not ok, want ok")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if target.display != "mayor" {
		t.Fatalf("display = %q, want mayor", target.display)
	}
	want := []string{"mayor"}
	if strings.Join(target.recipients, ",") != strings.Join(want, ",") {
		t.Fatalf("recipients = %#v, want %#v", target.recipients, want)
	}
}

func TestResolveMailTargetsForCommand_FailsWhenStoreBackedResolutionErrors(t *testing.T) {
	t.Setenv("GC_MAIL", "fake")

	prev := openMailTargetStore
	openMailTargetStore = func() (beads.Store, error) {
		return failingListByLabelStore{Store: beads.NewMemStore(), err: fmt.Errorf("boom")}, nil
	}
	t.Cleanup(func() {
		openMailTargetStore = prev
	})

	var stderr bytes.Buffer
	target, ok := resolveMailTargetsForCommand("sky", &stderr, "gc mail inbox")
	if ok {
		t.Fatalf("resolveMailTargetsForCommand() ok = true, want false; target=%#v", target)
	}
	if !strings.Contains(stderr.String(), "boom") {
		t.Fatalf("stderr = %q, want boom", stderr.String())
	}
}

func TestResolveMailTargetsForCommand_FailsWhenStoreOpenErrors(t *testing.T) {
	t.Setenv("GC_MAIL", "fake")

	prev := openMailTargetStore
	openMailTargetStore = func() (beads.Store, error) {
		return nil, fmt.Errorf("boom")
	}
	t.Cleanup(func() {
		openMailTargetStore = prev
	})

	var stderr bytes.Buffer
	target, ok := resolveMailTargetsForCommand("sky", &stderr, "gc mail inbox")
	if ok {
		t.Fatalf("resolveMailTargetsForCommand() ok = true, want false; target=%#v", target)
	}
	if !strings.Contains(stderr.String(), "boom") {
		t.Fatalf("stderr = %q, want boom", stderr.String())
	}
}

func TestConfiguredMailboxAddressDoesNotRequireProviderResolution(t *testing.T) {
	cityPath := t.TempDir()
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
provider = "missing-provider"

[[named_session]]
template = "mayor"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", cityPath)

	address, ok := configuredMailboxAddress("mayor")
	if !ok {
		t.Fatal("configuredMailboxAddress() = not ok, want ok")
	}
	if address != "mayor" {
		t.Fatalf("address = %q, want mayor", address)
	}
}

func TestConfiguredMailboxAddressResolvesQualifiedNamedSession(t *testing.T) {
	cityPath := t.TempDir()
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "witness"
dir = "demo"
provider = "missing-provider"

[[named_session]]
template = "witness"
dir = "demo"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", cityPath)

	address, ok := configuredMailboxAddress("demo/witness")
	if !ok {
		t.Fatal("configuredMailboxAddress() = not ok, want ok")
	}
	if address != "demo/witness" {
		t.Fatalf("address = %q, want demo/witness", address)
	}
}

func TestResolveMailRecipientIdentity_RejectsTemplatePrefixOnSessionSurface(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")

	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         "mayor",
			StartCommand: "true",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
		}},
	}

	_, err := resolveMailRecipientIdentity(t.TempDir(), cfg, store, "template:mayor")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolveMailRecipientIdentity(template:mayor) = %v, want ErrSessionNotFound", err)
	}

	all, err := store.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("session bead count = %d, want 0", len(all))
	}
}

func TestResolveMailRecipientIdentity_BareNamedSessionUsesConfiguredMailboxWithoutMaterializing(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")

	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         "mayor",
			StartCommand: "true",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
			Mode:     "always",
		}},
	}

	address, err := resolveMailRecipientIdentity(t.TempDir(), cfg, store, "mayor")
	if err != nil {
		t.Fatalf("resolveMailRecipientIdentity(mayor): %v", err)
	}
	if address != "mayor" {
		t.Fatalf("address = %q, want configured mailbox mayor", address)
	}

	all, err := store.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("session bead count = %d, want 0", len(all))
	}
}

func TestResolveMailRecipientIdentity_BareNamedSessionUsesExistingLiveMailboxWithoutMaterializing(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")

	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         "mayor",
			StartCommand: "true",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
			Mode:     "always",
		}},
	}
	existing, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "mayor",
			"session_name": "mayor",
			"template":     "mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create(existing session): %v", err)
	}

	address, err := resolveMailRecipientIdentity(t.TempDir(), cfg, store, "mayor")
	if err != nil {
		t.Fatalf("resolveMailRecipientIdentity(mayor): %v", err)
	}
	if address != "mayor" {
		t.Fatalf("address = %q, want existing live mailbox mayor", address)
	}

	all, err := store.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("session bead count = %d, want 1", len(all))
	}
	if all[0].ID != existing.ID {
		t.Fatalf("session bead ID = %q, want existing %q", all[0].ID, existing.ID)
	}
}

func TestResolveMailRecipientIdentity_BareRigScopedNamedUsesUniqueLiveConfiguredNamedSession(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")

	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         "rig-worker",
			Dir:          "frontend",
			StartCommand: "true",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "rig-worker",
			Dir:      "frontend",
			Mode:     "always",
		}},
	}

	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":                     "frontend/rig-worker",
			"session_name":              "frontend--rig-worker",
			"configured_named_session":  "true",
			"configured_named_identity": "frontend/rig-worker",
			"configured_named_mode":     "always",
		},
	}); err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	address, err := resolveMailRecipientIdentity(t.TempDir(), cfg, store, "rig-worker")
	if err != nil {
		t.Fatalf("resolveMailRecipientIdentity(rig-worker): %v", err)
	}
	if address != "frontend/rig-worker" {
		t.Fatalf("address = %q, want frontend/rig-worker", address)
	}

	all, err := store.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("session bead count = %d, want 1", len(all))
	}
}

func TestResolveMailRecipientIdentity_BareRigScopedNamedRejectsAmbiguousLiveConfiguredNamedSessions(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")

	store := beads.NewMemStore()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}

	for _, identity := range []string{"frontend/rig-worker", "backend/rig-worker"} {
		if _, err := store.Create(beads.Bead{
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"alias":                     identity,
				"session_name":              strings.ReplaceAll(identity, "/", "--"),
				"configured_named_session":  "true",
				"configured_named_identity": identity,
				"configured_named_mode":     "always",
			},
		}); err != nil {
			t.Fatalf("Create(%s): %v", identity, err)
		}
	}

	_, err := resolveMailRecipientIdentity(t.TempDir(), cfg, store, "rig-worker")
	if !errors.Is(err, session.ErrAmbiguous) {
		t.Fatalf("resolveMailRecipientIdentity(rig-worker) = %v, want ErrAmbiguous", err)
	}
}

// --- gc mail inbox ---

func TestCmdMailInbox_ManagedExecLifecycleProviderReadsInbox(t *testing.T) {
	cityDir, _ := setupManagedBdWaitTestCity(t)

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityDir, err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "managed exec session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "mayor",
			"alias":        "mayor",
			"template":     "worker",
			"state":        "asleep",
		},
	}); err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}
	mp := beadmail.New(store)
	if _, err := mp.Send("human", "mayor", "status", "hello from exec provider"); err != nil {
		t.Fatalf("mp.Send(): %v", err)
	}

	t.Setenv("GC_BEADS", "exec:"+gcBeadsBdScriptPath(cityDir))
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)

	var stdout, stderr bytes.Buffer
	if code := cmdMailInbox([]string{"mayor"}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdMailInbox() = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"FROM", "SUBJECT", "BODY", "human", "status", "hello from exec provider"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestCmdMailInbox_ManagedExecLifecycleProviderRecoversAfterHardKillPortRebind(t *testing.T) {
	cityDir, _ := setupManagedBdWaitTestCity(t)

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityDir, err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "managed exec session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "city-worker",
			"alias":        "city-worker",
			"template":     "worker",
			"state":        "asleep",
		},
	}); err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}
	mp := beadmail.New(store)
	if _, err := mp.Send("human", "city-worker", "status", "hello after managed rebind"); err != nil {
		t.Fatalf("mp.Send(): %v", err)
	}

	before, err := readDoltRuntimeStateFile(managedDoltStatePath(cityDir))
	if err != nil {
		t.Fatalf("readDoltRuntimeStateFile(before): %v", err)
	}
	if before.PID <= 0 || before.Port <= 0 {
		t.Fatalf("unexpected managed runtime before fault: %+v", before)
	}
	if err := syscall.Kill(before.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("Kill(%d): %v", before.PID, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for pidAlive(before.PID) && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}

	occupyManagedDoltPort(t, before.Port)

	var stdout, stderr bytes.Buffer
	if code := cmdMailInbox([]string{"city-worker"}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdMailInbox() = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if out := stdout.String(); !strings.Contains(out, "hello after managed rebind") {
		t.Fatalf("stdout missing recovered mail:\n%s", out)
	}

	var after doltRuntimeState
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		state, err := readDoltRuntimeStateFile(managedDoltStatePath(cityDir))
		if err == nil && state.Running && state.Port > 0 && state.Port != before.Port && state.PID > 0 && pidAlive(state.PID) {
			after = state
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if after.Port == 0 {
		after, err = readDoltRuntimeStateFile(managedDoltStatePath(cityDir))
		if err != nil {
			t.Fatalf("readDoltRuntimeStateFile(after): %v", err)
		}
		t.Fatalf("managed Dolt did not rebind after gc mail inbox recovery; before=%+v after=%+v", before, after)
	}
}

func TestMailInboxEmpty(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)

	var stdout, stderr bytes.Buffer
	code := doMailInbox(mp, "mayor", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailInbox = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No unread messages for mayor") {
		t.Errorf("stdout = %q, want no unread message", stdout.String())
	}
}

func TestMailInboxShowsMessages(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	mp.Send("human", "mayor", "", "hey there") //nolint:errcheck
	mp.Send("worker", "mayor", "", "status?")  //nolint:errcheck

	var stdout, stderr bytes.Buffer
	code := doMailInbox(mp, "mayor", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailInbox = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{"ID", "FROM", "BODY", "gc-1", "human", "hey there", "gc-2", "worker", "status?"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestMailInboxFiltersCorrectly(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	// Message to mayor (should appear).
	mp.Send("human", "mayor", "", "for mayor") //nolint:errcheck
	// Message to worker (should not appear in mayor's inbox).
	mp.Send("human", "worker", "", "for worker") //nolint:errcheck
	// Task bead (should not appear — wrong type).
	store.Create(beads.Bead{Title: "a task"}) //nolint:errcheck
	// Read message to mayor (should not appear — already read).
	m, _ := mp.Send("human", "mayor", "", "already read") //nolint:errcheck
	mp.Read(m.ID)                                         //nolint:errcheck

	var stdout bytes.Buffer
	code := doMailInbox(mp, "mayor", &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("doMailInbox = %d, want 0", code)
	}

	out := stdout.String()
	if !strings.Contains(out, "for mayor") {
		t.Errorf("stdout missing 'for mayor': %q", out)
	}
	if strings.Contains(out, "for worker") {
		t.Errorf("stdout should not contain 'for worker': %q", out)
	}
	if strings.Contains(out, "a task") {
		t.Errorf("stdout should not contain 'a task': %q", out)
	}
	if strings.Contains(out, "already read") {
		t.Errorf("stdout should not contain 'already read': %q", out)
	}
}

func TestMailInboxDefaultsToHuman(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	mp.Send("mayor", "human", "", "report") //nolint:errcheck

	var stdout bytes.Buffer
	code := doMailInbox(mp, "human", &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("doMailInbox = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "report") {
		t.Errorf("stdout = %q, want 'report'", stdout.String())
	}
}

// --- gc mail read ---

func TestMailReadSuccess(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	mp.Send("human", "mayor", "Hello", "hey, are you still there?") //nolint:errcheck

	var stdout, stderr bytes.Buffer
	code := doMailRead(mp, events.Discard, []string{"gc-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailRead = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{
		"ID:       gc-1",
		"From:     human",
		"To:       mayor",
		"Subject:  Hello",
		"Body:     hey, are you still there?",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}

	// Verify bead is still open (read only adds "read" label, NOT closed).
	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "open" {
		t.Errorf("bead Status = %q, want %q (read must not close)", b.Status, "open")
	}
}

func TestMailReadMissingID(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)

	var stderr bytes.Buffer
	code := doMailRead(mp, events.Discard, nil, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doMailRead = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "missing message ID") {
		t.Errorf("stderr = %q, want 'missing message ID'", stderr.String())
	}
}

func TestMailReadNotFound(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)

	var stderr bytes.Buffer
	code := doMailRead(mp, events.Discard, []string{"gc-999"}, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doMailRead = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "bead not found") {
		t.Errorf("stderr = %q, want 'bead not found'", stderr.String())
	}
}

func TestMailReadAlreadyRead(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	mp.Send("human", "mayor", "", "old news") //nolint:errcheck
	mp.Read("gc-1")                           //nolint:errcheck

	// Reading an already-read message should still display it without error.
	var stdout, stderr bytes.Buffer
	code := doMailRead(mp, events.Discard, []string{"gc-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailRead = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "old news") {
		t.Errorf("stdout = %q, want 'old news'", stdout.String())
	}
}

// --- gc mail peek ---

func TestMailPeekSuccess(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	mp.Send("human", "mayor", "Hello", "peek body") //nolint:errcheck

	var stdout, stderr bytes.Buffer
	code := doMailPeek(mp, []string{"gc-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailPeek = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "peek body") {
		t.Errorf("stdout missing 'peek body':\n%s", out)
	}

	// Message should still be in inbox (not marked read).
	msgs, _ := mp.Inbox("mayor")
	if len(msgs) != 1 {
		t.Errorf("Inbox after peek = %d, want 1", len(msgs))
	}
}

func TestMailPeekMissingID(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)

	var stderr bytes.Buffer
	code := doMailPeek(mp, nil, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doMailPeek = %d, want 1", code)
	}
}

// --- gc mail reply ---

func TestMailReplySuccess(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	mp.Send("alice", "bob", "Hello", "first") //nolint:errcheck

	var stdout, stderr bytes.Buffer
	code := doMailReply(mp, events.Discard, "gc-1", "bob", "RE: Hello", "reply body", nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailReply = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Replied to gc-1") {
		t.Errorf("stdout = %q, want reply confirmation", stdout.String())
	}
	if !strings.Contains(stdout.String(), "to alice") {
		t.Errorf("stdout = %q, want reply addressed to alice", stdout.String())
	}
}

func TestMailReplyNotifySuccess(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	mp.Send("alice", "bob", "Hello", "first") //nolint:errcheck

	var nudged string
	nf := func(recipient string) error {
		nudged = recipient
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := doMailReply(mp, events.Discard, "gc-1", "bob", "RE: Hello", "reply body", nf, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailReply = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Replied to gc-1") {
		t.Errorf("stdout = %q, want reply confirmation", stdout.String())
	}
	if nudged != "alice" {
		t.Errorf("nudgeFn called with %q, want %q", nudged, "alice")
	}
}

func TestMailReplyNotifyNudgeError(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	mp.Send("alice", "bob", "Hello", "first") //nolint:errcheck

	nf := func(_ string) error {
		return fmt.Errorf("session not found")
	}

	var stdout, stderr bytes.Buffer
	code := doMailReply(mp, events.Discard, "gc-1", "bob", "RE: Hello", "reply body", nf, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailReply = %d, want 0 (nudge failure is non-fatal); stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Replied to gc-1") {
		t.Errorf("stdout = %q, want reply confirmation", stdout.String())
	}
	if !strings.Contains(stderr.String(), "nudge failed") {
		t.Errorf("stderr = %q, want nudge failure warning", stderr.String())
	}
}

func TestCmdMailReply_FallsBackToGCSessionIDWhenAliasMissing(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
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
	sessionBead, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "",
			"session_name": "codeprobe-worker-gc-1941",
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	mp := beadmail.New(store)
	if _, err := mp.Send("alice", sessionBead.ID, "Hello", "first"); err != nil {
		t.Fatalf("mp.Send(): %v", err)
	}

	t.Setenv("GC_ALIAS", "codeprobe-worker-1")
	t.Setenv("GC_SESSION_ID", "codeprobe-worker-gc-1941")
	t.Setenv("GC_AGENT", "codeprobe-worker")

	var stdout, stderr bytes.Buffer
	code := cmdMailReply([]string{"gc-2", "reply body"}, "", "", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdMailReply() = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Replied to gc-2") {
		t.Fatalf("stdout = %q, want reply confirmation", stdout.String())
	}
	if !strings.Contains(stdout.String(), "to alice") {
		t.Fatalf("stdout = %q, want reply addressed to alice", stdout.String())
	}
}

func TestCmdMailReplyHumanNotifyQueuesNudge(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_MAIL", "")
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_AGENT", "")

	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", cityPath)

	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	sessionBead, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "alice",
			"session_name": "alice-session",
			"provider":     "fake",
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	mp := beadmail.New(store)
	original, err := mp.Send("alice", "human", "Hello", "first")
	if err != nil {
		t.Fatalf("mp.Send(): %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdMailReply([]string{original.ID, "reply body"}, "", "", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdMailReply() = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "to alice") {
		t.Fatalf("stdout = %q, want reply addressed to alice", stdout.String())
	}

	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatalf("LoadState(): %v", err)
	}
	if len(state.Pending) != 1 {
		t.Fatalf("pending nudges = %d, want 1; state=%+v stderr=%s", len(state.Pending), state, stderr.String())
	}
	nudge := state.Pending[0]
	if nudge.Agent != "alice" {
		t.Fatalf("nudge.Agent = %q, want alice", nudge.Agent)
	}
	if nudge.SessionID != sessionBead.ID {
		t.Fatalf("nudge.SessionID = %q, want %q", nudge.SessionID, sessionBead.ID)
	}
	if nudge.Source != "mail" {
		t.Fatalf("nudge.Source = %q, want mail", nudge.Source)
	}
	if nudge.Message != "You have mail from human" {
		t.Fatalf("nudge.Message = %q", nudge.Message)
	}
}

func TestCmdMailReplyExecProviderNotifyQueuesNudge(t *testing.T) {
	cityPath, sessionID, script := setupExecMailReplyNudgeTest(t)
	t.Setenv("GC_MAIL", "exec:"+script)

	var stdout, stderr bytes.Buffer
	code := cmdMailReply([]string{"gc-1", "reply body"}, "", "", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdMailReply() = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	assertQueuedMailNudge(t, cityPath, sessionID, stderr.String())
}

func TestMailReplyNudgeAliasQueuesNudge(t *testing.T) {
	cityPath, sessionID, script := setupExecMailReplyNudgeTest(t)
	t.Setenv("GC_MAIL", "exec:"+script)

	var stdout, stderr bytes.Buffer
	cmd := newMailReplyCmd(&stdout, &stderr)
	if cmd.Flags().Lookup("nudge") == nil {
		t.Fatal("reply command missing --nudge alias")
	}
	cmd.SetArgs([]string{"gc-1", "--nudge", "reply body"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("reply --nudge: %v; stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}

	assertQueuedMailNudge(t, cityPath, sessionID, stderr.String())
}

func TestCmdMailReplyExecProviderNotifyWithoutCityWarnsAndSendsReply(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_MAIL", "exec:"+writeExecReplyScript(t))
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_AGENT", "")
	t.Chdir(t.TempDir())

	var stdout, stderr bytes.Buffer
	code := cmdMailReply([]string{"gc-1", "reply body"}, "", "", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdMailReply() = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Replied to gc-1") {
		t.Fatalf("stdout = %q, want reply confirmation", stdout.String())
	}
	if !strings.Contains(stderr.String(), "--notify requested but no city store available") {
		t.Fatalf("stderr = %q, want notify warning", stderr.String())
	}
}

func TestCmdMailReplyExecProviderNotifyResolvesNonHumanSender(t *testing.T) {
	cityPath, sessionID, script := setupExecMailReplyNudgeTest(t)
	t.Setenv("GC_MAIL", "exec:"+script)
	t.Setenv("GC_SESSION_ID", "bob-session")

	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "bob",
			"session_name": "bob-session",
			"provider":     "fake",
		},
	}); err != nil {
		t.Fatalf("Create(sender session): %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdMailReply([]string{"gc-1", "reply body"}, "", "", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdMailReply() = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	assertQueuedMailNudgeMessage(t, cityPath, sessionID, "You have mail from bob", stderr.String())
}

func setupExecMailReplyNudgeTest(t *testing.T) (string, string, string) {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_AGENT", "")

	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", cityPath)
	t.Setenv("GC_CITY_PATH", cityPath)

	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	sessionBead, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "alice",
			"session_name": "alice-session",
			"provider":     "fake",
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	return cityPath, sessionBead.ID, writeExecReplyScript(t)
}

func writeExecReplyScript(t *testing.T) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "mail-exec")
	data := `#!/bin/sh
case "$1" in
  ensure-running)
    exit 0
    ;;
  reply)
    cat >/dev/null
    printf '{"id":"exec-reply-1","from":"human","to":"alice","subject":"RE: Hello","body":"reply body","created_at":"2026-04-28T00:00:00Z","read":false,"thread_id":"thread-1","reply_to":"%s"}\n' "$2"
    exit 0
    ;;
  *)
    exit 2
    ;;
esac
`
	if err := os.WriteFile(script, []byte(data), 0o755); err != nil {
		t.Fatalf("WriteFile(exec script): %v", err)
	}
	return script
}

func assertQueuedMailNudge(t *testing.T, cityPath, sessionID, stderr string) {
	t.Helper()
	assertQueuedMailNudgeMessage(t, cityPath, sessionID, "You have mail from human", stderr)
}

func assertQueuedMailNudgeMessage(t *testing.T, cityPath, sessionID, message, stderr string) {
	t.Helper()
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatalf("LoadState(): %v", err)
	}
	if len(state.Pending) != 1 {
		t.Fatalf("pending nudges = %d, want 1; state=%+v stderr=%s", len(state.Pending), state, stderr)
	}
	nudge := state.Pending[0]
	if nudge.Agent != "alice" {
		t.Fatalf("nudge.Agent = %q, want alice", nudge.Agent)
	}
	if nudge.SessionID != sessionID {
		t.Fatalf("nudge.SessionID = %q, want %q", nudge.SessionID, sessionID)
	}
	if nudge.Source != "mail" {
		t.Fatalf("nudge.Source = %q, want mail", nudge.Source)
	}
	if nudge.Message != message {
		t.Fatalf("nudge.Message = %q", nudge.Message)
	}
}

// --- gc mail mark-read / mark-unread ---

func TestMailMarkReadSuccess(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	mp.Send("human", "mayor", "", "mark me") //nolint:errcheck

	var stdout, stderr bytes.Buffer
	code := doMailMarkRead(mp, events.Discard, []string{"gc-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailMarkRead = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Marked gc-1 as read") {
		t.Errorf("stdout = %q, want confirmation", stdout.String())
	}
}

func TestMailMarkUnreadSuccess(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	mp.Send("human", "mayor", "", "unmark me") //nolint:errcheck
	mp.MarkRead("gc-1")                        //nolint:errcheck

	var stdout, stderr bytes.Buffer
	code := doMailMarkUnread(mp, events.Discard, []string{"gc-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailMarkUnread = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Marked gc-1 as unread") {
		t.Errorf("stdout = %q, want confirmation", stdout.String())
	}
}

// --- gc mail delete ---

func TestMailDeleteSuccess(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	mp.Send("human", "mayor", "", "delete me") //nolint:errcheck

	var stdout, stderr bytes.Buffer
	code := doMailDelete(mp, events.Discard, []string{"gc-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailDelete = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Deleted message gc-1") {
		t.Errorf("stdout = %q, want deletion confirmation", stdout.String())
	}
}

func TestMailDeleteMultiSuccess(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	for i := 0; i < 3; i++ {
		if _, err := mp.Send("human", "mayor", "", "batch me"); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}

	var stdout, stderr bytes.Buffer
	rec := &memRecorder{}
	code := doMailDelete(mp, rec, []string{"gc-1", "gc-2", "gc-3"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailDelete = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"Deleted message gc-1", "Deleted message gc-2", "Deleted message gc-3"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
	if n := len(rec.events); n != 3 {
		t.Errorf("recorded events = %d, want 3", n)
	}
	for _, id := range []string{"gc-1", "gc-2", "gc-3"} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if b.Status != "closed" {
			t.Errorf("bead %s Status = %q, want closed", id, b.Status)
		}
	}
}

func TestMailDeleteMultiPartialFailure(t *testing.T) {
	mp := mail.NewFake()
	m1, _ := mp.Send("human", "mayor", "", "one")
	m2, _ := mp.Send("human", "mayor", "", "two")
	if err := mp.Archive(m2.ID); err != nil {
		t.Fatalf("pre-archive m2: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := doMailDelete(mp, events.Discard, []string{m1.ID, m2.ID, "ghost"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doMailDelete = %d, want 1; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Deleted message "+m1.ID) {
		t.Errorf("stdout missing Deleted for m1:\n%s", out)
	}
	if !strings.Contains(out, "Already deleted "+m2.ID) {
		t.Errorf("stdout missing Already deleted for m2:\n%s", out)
	}
	if !strings.Contains(stderr.String(), "gc mail delete ghost") {
		t.Errorf("stderr missing per-id error for ghost:\n%s", stderr.String())
	}
}

func TestMailDeleteMultiExecProviderUsesDeleteCommand(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ops.log")
	scriptPath := filepath.Join(dir, "mail-provider")
	script := fmt.Sprintf(`#!/bin/sh
set -eu
op="$1"
case "$op" in
  ensure-running)
    ;;
  archive|delete)
    printf '%%s %%s\n' "$op" "$2" >> %q
    ;;
  *)
    echo "unexpected op $op" >&2
    exit 2
    ;;
esac
`, logPath)
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(script): %v", err)
	}

	mp := mailexec.NewProvider(scriptPath)
	var stdout, stderr bytes.Buffer
	code := doMailDelete(mp, events.Discard, []string{"msg-1", "msg-2"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailDelete = %d, want 0; stderr: %s", code, stderr.String())
	}
	gotBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile(log): %v", err)
	}
	want := "delete msg-1\ndelete msg-2\n"
	if got := string(gotBytes); got != want {
		t.Fatalf("exec operations = %q, want %q", got, want)
	}
}

// --- gc mail thread ---

func TestMailThreadSuccess(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	sent, _ := mp.Send("alice", "bob", "Hello", "first") //nolint:errcheck
	mp.Reply(sent.ID, "bob", "RE: Hello", "second")      //nolint:errcheck

	var stdout, stderr bytes.Buffer
	code := doMailThread(mp, []string{sent.ThreadID}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailThread = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "alice") {
		t.Errorf("stdout missing 'alice':\n%s", out)
	}
	if !strings.Contains(out, "bob") {
		t.Errorf("stdout missing 'bob':\n%s", out)
	}
}

func TestMailThreadEmpty(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)

	var stdout bytes.Buffer
	code := doMailThread(mp, []string{"nonexistent"}, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("doMailThread = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "No messages in thread") {
		t.Errorf("stdout = %q, want empty thread message", stdout.String())
	}
}

// --- gc mail count ---

func TestMailCountSuccess(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	mp.Send("alice", "bob", "", "msg1") //nolint:errcheck
	m2, _ := mp.Send("alice", "bob", "", "msg2")
	mp.MarkRead(m2.ID) //nolint:errcheck

	var stdout, stderr bytes.Buffer
	code := doMailCount(mp, "bob", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailCount = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "2 total, 1 unread for bob") {
		t.Errorf("stdout = %q, want count output", stdout.String())
	}
}

func TestMailCountTargetIncludesHistoricalAliases(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	b, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "sky",
			"alias_history": "mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	if _, err := mp.Send("human", "sky", "", "current"); err != nil {
		t.Fatalf("Send(current): %v", err)
	}
	oldMsg, err := mp.Send("human", "mayor", "", "old alias")
	if err != nil {
		t.Fatalf("Send(old): %v", err)
	}
	if _, err := mp.Send("human", b.ID, "", "session id"); err != nil {
		t.Fatalf("Send(id): %v", err)
	}
	if err := mp.MarkRead(oldMsg.ID); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}

	target, err := resolveMailTargets(store, "sky")
	if err != nil {
		t.Fatalf("resolveMailTargets: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := doMailCountTarget(mp, target, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailCountTarget = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "3 total, 2 unread for sky") {
		t.Fatalf("stdout = %q, want merged historical count", stdout.String())
	}
}

func TestMailCountTargetUsesCountPerRecipient(t *testing.T) {
	target := resolvedMailTarget{
		display:    "sky",
		recipients: []string{"sky", "gc-1"},
	}

	var stdout, stderr bytes.Buffer
	code := doMailCountTarget(countOnlyMailProvider{}, target, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailCountTarget = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "3 total, 2 unread for sky") {
		t.Fatalf("stdout = %q, want merged count output", stdout.String())
	}
}

// --- gc mail archive ---

func TestMailArchiveSuccess(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	mp.Send("human", "mayor", "", "dismiss me") //nolint:errcheck

	var stdout, stderr bytes.Buffer
	code := doMailArchive(mp, events.Discard, []string{"gc-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailArchive = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Archived message gc-1") {
		t.Errorf("stdout = %q, want archived confirmation", stdout.String())
	}

	// Verify bead is now closed.
	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "closed" {
		t.Errorf("bead Status = %q, want %q", b.Status, "closed")
	}
}

func TestMailArchiveMissingID(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)

	var stderr bytes.Buffer
	code := doMailArchive(mp, events.Discard, nil, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doMailArchive = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "missing message ID") {
		t.Errorf("stderr = %q, want 'missing message ID'", stderr.String())
	}
}

func TestMailArchiveNotFound(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)

	var stderr bytes.Buffer
	code := doMailArchive(mp, events.Discard, []string{"gc-999"}, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doMailArchive = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "bead not found") {
		t.Errorf("stderr = %q, want 'bead not found'", stderr.String())
	}
}

func TestMailArchiveNonMessage(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	store.Create(beads.Bead{Title: "a task"}) //nolint:errcheck // Type defaults to "" (task)

	var stderr bytes.Buffer
	code := doMailArchive(mp, events.Discard, []string{"gc-1"}, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doMailArchive = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not a message") {
		t.Errorf("stderr = %q, want 'not a message'", stderr.String())
	}
}

func TestMailArchiveAlreadyClosed(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	mp.Send("human", "mayor", "", "old") //nolint:errcheck
	mp.Archive("gc-1")                   //nolint:errcheck

	var stdout, stderr bytes.Buffer
	code := doMailArchive(mp, events.Discard, []string{"gc-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailArchive = %d, want 0; stderr: %s", code, stderr.String())
	}
	// Already-closed messages report as already archived.
	if !strings.Contains(stdout.String(), "Already archived gc-1") {
		t.Errorf("stdout = %q, want 'Already archived'", stdout.String())
	}
}

func TestMailArchiveMultiSuccess(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	for i := 0; i < 3; i++ {
		if _, err := mp.Send("human", "mayor", "", "batch"); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}

	var stdout, stderr bytes.Buffer
	rec := &memRecorder{}
	code := doMailArchive(mp, rec, []string{"gc-1", "gc-2", "gc-3"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailArchive = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"Archived message gc-1", "Archived message gc-2", "Archived message gc-3"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
	if n := len(rec.events); n != 3 {
		t.Errorf("recorded events = %d, want 3", n)
	}
}

func TestMailArchiveMultiPartialFailure(t *testing.T) {
	mp := mail.NewFake()
	m1, _ := mp.Send("human", "mayor", "", "one")
	m2, _ := mp.Send("human", "mayor", "", "two")
	if err := mp.Archive(m2.ID); err != nil {
		t.Fatalf("pre-archive: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := doMailArchive(mp, events.Discard, []string{m1.ID, m2.ID, "ghost"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doMailArchive = %d, want 1; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Archived message "+m1.ID) {
		t.Errorf("stdout missing Archived for m1:\n%s", out)
	}
	if !strings.Contains(out, "Already archived "+m2.ID) {
		t.Errorf("stdout missing Already archived for m2:\n%s", out)
	}
	if !strings.Contains(stderr.String(), "gc mail archive ghost") {
		t.Errorf("stderr missing per-id error for ghost:\n%s", stderr.String())
	}
}

// --- gc mail send --notify ---

func TestMailSendNotifySuccess(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	recipients := map[string]bool{"human": true, "mayor": true}

	var nudged string
	nf := func(recipient string) error {
		nudged = recipient
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := doMailSend(mp, events.Discard, recipients, "human", []string{"mayor", "wake up"}, nf, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailSend = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Sent message gc-1 to mayor") {
		t.Errorf("stdout = %q, want sent confirmation", stdout.String())
	}
	if nudged != "mayor" {
		t.Errorf("nudgeFn called with %q, want %q", nudged, "mayor")
	}
}

func TestMailSendNotifyNudgeError(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	recipients := map[string]bool{"human": true, "mayor": true}

	nf := func(_ string) error {
		return fmt.Errorf("session not found")
	}

	var stdout, stderr bytes.Buffer
	code := doMailSend(mp, events.Discard, recipients, "human", []string{"mayor", "wake up"}, nf, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailSend = %d, want 0 (nudge failure is non-fatal); stderr: %s", code, stderr.String())
	}
	// Mail should still be sent.
	if !strings.Contains(stdout.String(), "Sent message gc-1 to mayor") {
		t.Errorf("stdout = %q, want sent confirmation", stdout.String())
	}
	// Warning should appear on stderr.
	if !strings.Contains(stderr.String(), "nudge failed") {
		t.Errorf("stderr = %q, want nudge failure warning", stderr.String())
	}
}

func TestMailSendNotifyToHuman(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	recipients := map[string]bool{"human": true, "mayor": true}

	nudgeCalled := false
	nf := func(_ string) error {
		nudgeCalled = true
		return nil
	}

	var stdout bytes.Buffer
	code := doMailSend(mp, events.Discard, recipients, "mayor", []string{"human", "done"}, nf, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("doMailSend = %d, want 0", code)
	}
	if nudgeCalled {
		t.Error("nudgeFn should not be called when recipient is human")
	}
}

func TestMailSendWithoutNotify(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	recipients := map[string]bool{"human": true, "mayor": true}

	var stdout, stderr bytes.Buffer
	code := doMailSend(mp, events.Discard, recipients, "human", []string{"mayor", "no nudge"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailSend = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Sent message gc-1 to mayor") {
		t.Errorf("stdout = %q, want sent confirmation", stdout.String())
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}
}

// --- gc mail send -s/-m ---

func TestMailSendSubjectFlag(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	recipients := map[string]bool{"human": true, "mayor": true}

	// Simulate -s flag: args = [to, subject, body].
	var stdout bytes.Buffer
	code := doMailSend(mp, events.Discard, recipients, "human", []string{"mayor", "Build is green", ""}, nil, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("doMailSend = %d, want 0", code)
	}

	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Title != "Build is green" {
		t.Errorf("bead Title = %q, want %q", b.Title, "Build is green")
	}
}

func TestMailSendSubjectAndMessage(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	recipients := map[string]bool{"human": true, "mayor": true}

	// args = [to, subject, body] from -s/-m flags.
	var stdout bytes.Buffer
	code := doMailSend(mp, events.Discard, recipients, "witness", []string{"mayor", "ESCALATION: Auth broken", "Token refresh fails after 30min"}, nil, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("doMailSend = %d, want 0", code)
	}

	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Title != "ESCALATION: Auth broken" {
		t.Errorf("bead Title = %q, want %q", b.Title, "ESCALATION: Auth broken")
	}
	if b.Description != "Token refresh fails after 30min" {
		t.Errorf("bead Description = %q, want %q", b.Description, "Token refresh fails after 30min")
	}
}

// --- gc mail send --from ---

func TestMailSendFromFlag(t *testing.T) {
	// --from sets the sender field on the created bead.
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	recipients := map[string]bool{"human": true, "mayor": true}

	var stdout bytes.Buffer
	code := doMailSend(mp, events.Discard, recipients, "deacon", []string{"mayor", "patrol complete"}, nil, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("doMailSend = %d, want 0", code)
	}

	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.From != "deacon" {
		t.Errorf("bead From = %q, want %q", b.From, "deacon")
	}
}

// --- gc mail send --to ---

func TestMailSendToFlag(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	recipients := map[string]bool{"human": true, "mayor": true}

	var stdout, stderr bytes.Buffer
	code := doMailSend(mp, events.Discard, recipients, "human", []string{"mayor", "hello from --to"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailSend = %d, want 0; stderr: %s", code, stderr.String())
	}

	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Assignee != "mayor" {
		t.Errorf("bead Assignee = %q, want %q", b.Assignee, "mayor")
	}
}

func TestMailSendAcceptsNudgeAlias(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newMailSendCmd(&stdout, &stderr)
	if cmd.Flags().Lookup("nudge") == nil {
		t.Fatal("send command missing --nudge alias")
	}
	if err := cmd.Flags().Set("nudge", "true"); err != nil {
		t.Fatalf("set --nudge: %v", err)
	}
}

// --- gc mail send --all ---

func TestMailSendAll(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	recipients := map[string]bool{"human": true, "coder": true, "committer": true, "tester": true}

	var stdout, stderr bytes.Buffer
	code := doMailSendAll(mp, events.Discard, recipients, "coder", []string{"status update: tests passing"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailSendAll = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}

	out := stdout.String()
	// Should send to committer and tester (not coder/sender, not human).
	if !strings.Contains(out, "Sent message gc-1 to committer") {
		t.Errorf("stdout missing committer send:\n%s", out)
	}
	if !strings.Contains(out, "Sent message gc-2 to tester") {
		t.Errorf("stdout missing tester send:\n%s", out)
	}
	if strings.Contains(out, "to coder") {
		t.Errorf("stdout should not contain send to sender (coder):\n%s", out)
	}
	if strings.Contains(out, "to human") {
		t.Errorf("stdout should not contain send to human:\n%s", out)
	}
}

func TestMailSendAllMissingBody(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	recipients := map[string]bool{"human": true, "coder": true}

	var stderr bytes.Buffer
	code := doMailSendAll(mp, events.Discard, recipients, "human", nil, nil, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doMailSendAll = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("stderr = %q, want usage message", stderr.String())
	}
}

func TestMailSendAllNoRecipients(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	// Only human and sender — no one to broadcast to.
	recipients := map[string]bool{"human": true, "coder": true}

	var stderr bytes.Buffer
	code := doMailSendAll(mp, events.Discard, recipients, "coder", []string{"hello?"}, nil, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doMailSendAll = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no recipients") {
		t.Errorf("stderr = %q, want 'no recipients'", stderr.String())
	}
}

func TestMailSendAllExcludesSender(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	recipients := map[string]bool{"human": true, "alice": true, "bob": true}

	var stdout bytes.Buffer
	code := doMailSendAll(mp, events.Discard, recipients, "alice", []string{"broadcast"}, nil, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("doMailSendAll = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "to bob") {
		t.Errorf("stdout missing send to bob:\n%s", out)
	}
	if strings.Contains(out, "to alice") {
		t.Errorf("stdout should not contain send to sender alice:\n%s", out)
	}
}

// --- gc mail check ---

func TestMailCheckNoMail(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)

	var stdout, stderr bytes.Buffer
	code := doMailCheck(mp, "mayor", false, &stdout, &stderr)
	if code != 1 {
		t.Errorf("doMailCheck = %d, want 1 (no mail)", code)
	}
	if stdout.Len() > 0 {
		t.Errorf("unexpected stdout: %q", stdout.String())
	}
}

func TestMailCheckHasMail(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	mp.Send("human", "mayor", "", "hey") //nolint:errcheck
	mp.Send("worker", "mayor", "", "yo") //nolint:errcheck

	var stdout, stderr bytes.Buffer
	code := doMailCheck(mp, "mayor", false, &stdout, &stderr)
	if code != 0 {
		t.Errorf("doMailCheck = %d, want 0 (has mail)", code)
	}
	if !strings.Contains(stdout.String(), "2 unread message(s) for mayor") {
		t.Errorf("stdout = %q, want count message", stdout.String())
	}
}

func TestMailInboxTargetIncludesHistoricalAliases(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	b, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "sky",
			"alias_history": "mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	if _, err := mp.Send("human", "sky", "", "current alias"); err != nil {
		t.Fatalf("Send(current): %v", err)
	}
	if _, err := mp.Send("human", "mayor", "", "historical alias"); err != nil {
		t.Fatalf("Send(old): %v", err)
	}
	if _, err := mp.Send("human", b.ID, "", "session id"); err != nil {
		t.Fatalf("Send(id): %v", err)
	}

	target, err := resolveMailTargets(store, "sky")
	if err != nil {
		t.Fatalf("resolveMailTargets: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := doMailInboxTarget(mp, target, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailInboxTarget = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"current alias", "historical alias", "session id"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestMailCheckInjectNoMail(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)

	var stdout, stderr bytes.Buffer
	code := doMailCheck(mp, "mayor", true, &stdout, &stderr)
	if code != 0 {
		t.Errorf("doMailCheck = %d, want 0 (--inject always exits 0)", code)
	}
	if stdout.Len() > 0 {
		t.Errorf("unexpected stdout: %q (should be silent when no mail)", stdout.String())
	}
}

func TestMailCheckInjectFormatsMessages(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	mp.Send("mayor", "worker", "", "Fix the auth bug")          //nolint:errcheck
	mp.Send("polecat", "worker", "", "PR #17 ready for review") //nolint:errcheck

	var stdout, stderr bytes.Buffer
	code := doMailCheck(mp, "worker", true, &stdout, &stderr)
	if code != 0 {
		t.Errorf("doMailCheck = %d, want 0", code)
	}

	out := stdout.String()
	if !strings.Contains(out, "<system-reminder>") {
		t.Errorf("stdout missing <system-reminder> tag:\n%s", out)
	}
	if !strings.Contains(out, "</system-reminder>") {
		t.Errorf("stdout missing </system-reminder> tag:\n%s", out)
	}
	if !strings.Contains(out, "2 unread message(s)") {
		t.Errorf("stdout missing message count:\n%s", out)
	}
	if !strings.Contains(out, "gc-1 from mayor: Fix the auth bug") {
		t.Errorf("stdout missing first message:\n%s", out)
	}
	if !strings.Contains(out, "gc-2 from polecat: PR #17 ready for review") {
		t.Errorf("stdout missing second message:\n%s", out)
	}
	if !strings.Contains(out, "gc mail read <id>") {
		t.Errorf("stdout missing read hint:\n%s", out)
	}
}

func TestMailCheckInjectLimitsMessageCount(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	mp.Send("sender-a", "recipient", "", "first")  //nolint:errcheck
	mp.Send("sender-b", "recipient", "", "second") //nolint:errcheck
	mp.Send("sender-c", "recipient", "", "third")  //nolint:errcheck
	mp.Send("sender-d", "recipient", "", "fourth") //nolint:errcheck

	var stdout bytes.Buffer
	code := doMailCheck(mp, "recipient", true, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("doMailCheck = %d, want 0", code)
	}

	out := stdout.String()
	for _, want := range []string{"4 unread message(s)", "gc-1 from sender-a", "gc-2 from sender-b", "gc-3 from sender-c", "Showing the first 3 message(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "gc-4") || strings.Contains(out, "fourth") {
		t.Errorf("stdout should not include the fourth message:\n%s", out)
	}
}

func TestMailCheckInjectTruncatesLongBodies(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	longBody := "prefix " + strings.Repeat("x", mailInjectBodyPreviewSize+100)
	mp.Send("sender-a", "recipient", "Long body", longBody) //nolint:errcheck

	var stdout bytes.Buffer
	code := doMailCheck(mp, "recipient", true, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("doMailCheck = %d, want 0", code)
	}

	out := stdout.String()
	if !strings.Contains(out, "Long body") {
		t.Errorf("stdout missing subject:\n%s", out)
	}
	if !strings.Contains(out, "... [preview truncated]") {
		t.Errorf("stdout missing truncation marker:\n%s", out)
	}
	if strings.Contains(out, strings.Repeat("x", mailInjectBodyPreviewSize+80)) {
		t.Errorf("stdout includes too much of the long body:\n%s", out)
	}
}

func TestMailCheckInjectCompactsAndBoundsLongSubjects(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	longSubject := "subject\n\tline " + strings.Repeat("x", mailInjectBodyPreviewSize+100) + " tail"
	mp.Send("sender-a", "recipient", longSubject, "short body") //nolint:errcheck

	var stdout bytes.Buffer
	code := doMailCheck(mp, "recipient", true, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("doMailCheck = %d, want 0", code)
	}

	out := stdout.String()
	if !strings.Contains(out, "[subject line ") {
		t.Fatalf("stdout missing compacted subject prefix:\n%s", out)
	}
	if strings.Contains(out, "subject\n\tline") {
		t.Fatalf("stdout contains raw multiline subject:\n%s", out)
	}
	if strings.Contains(out, strings.Repeat("x", mailInjectBodyPreviewSize+80)) {
		t.Fatalf("stdout includes too much of the long subject:\n%s", out)
	}
	if !strings.Contains(out, "... [subject truncated]") {
		t.Fatalf("stdout missing subject truncation marker:\n%s", out)
	}
}

func TestMailCheckInjectOmitsSubjectWhenFullBodyMatches(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	longBody := strings.Repeat("x", mailInjectBodyPreviewSize+100)
	mp.Send("sender-a", "recipient", longBody, longBody) //nolint:errcheck

	var stdout bytes.Buffer
	code := doMailCheck(mp, "recipient", true, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("doMailCheck = %d, want 0", code)
	}

	out := stdout.String()
	if strings.Contains(out, "["+longBody+"]") {
		t.Errorf("stdout should not repeat a matching subject after body truncation:\n%s", out)
	}
	if !strings.Contains(out, "gc-1 from sender-a: ") {
		t.Errorf("stdout missing compact message format:\n%s", out)
	}
	if !strings.Contains(out, "... [preview truncated]") {
		t.Errorf("stdout missing truncation marker:\n%s", out)
	}
}

func TestMailInjectBodyPreviewUsesBoundedScan(t *testing.T) {
	body := strings.Repeat(" ", mailInjectPreviewScanSize+1) + "tail"
	preview, truncated := mailInjectBodyPreview(body)
	if !truncated {
		t.Fatalf("mailInjectBodyPreview did not truncate after scan budget")
	}
	if preview != "" {
		t.Fatalf("mailInjectBodyPreview = %q, want empty preview after leading-space budget", preview)
	}
}

func TestMailInjectBodyPreviewCompactsWhitespace(t *testing.T) {
	preview, truncated := mailInjectBodyPreview(" first\n\tsecond   third ")
	if truncated {
		t.Fatalf("mailInjectBodyPreview truncated short body")
	}
	if preview != "first second third" {
		t.Fatalf("mailInjectBodyPreview = %q, want %q", preview, "first second third")
	}
}

func TestMailInjectBodyPreviewKeepsUTF8Boundary(t *testing.T) {
	prefix := strings.Repeat("a", mailInjectBodyPreviewSize-1)
	compact := prefix + "界tail"

	preview, truncated := mailInjectBodyPreview(compact)
	if !truncated {
		t.Fatalf("mailInjectBodyPreview did not truncate long body")
	}
	if preview != prefix {
		t.Fatalf("mailInjectBodyPreview = %q, want %q", preview, prefix)
	}
	if !utf8.ValidString(preview) {
		t.Fatalf("mailInjectBodyPreview returned invalid UTF-8: %q", preview)
	}
}

func TestMailCheckInjectDoesNotCloseBeads(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	mp.Send("human", "mayor", "", "still open") //nolint:errcheck

	var stdout bytes.Buffer
	code := doMailCheck(mp, "mayor", true, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("doMailCheck = %d, want 0", code)
	}

	// Bead must remain open after injection.
	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "open" {
		t.Errorf("bead Status = %q, want %q (inject must not close beads)", b.Status, "open")
	}
}

func TestMailCheckInjectFiltersCorrectly(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	// Message to mayor (should appear).
	mp.Send("human", "mayor", "", "for mayor") //nolint:errcheck
	// Message to worker (should not appear in mayor's check).
	mp.Send("human", "worker", "", "for worker") //nolint:errcheck
	// Task bead (should not appear — wrong type).
	store.Create(beads.Bead{Title: "a task"}) //nolint:errcheck
	// Read message to mayor (should not appear).
	m, _ := mp.Send("human", "mayor", "", "already read") //nolint:errcheck
	mp.Read(m.ID)                                         //nolint:errcheck

	var stdout bytes.Buffer
	code := doMailCheck(mp, "mayor", true, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("doMailCheck = %d, want 0", code)
	}

	out := stdout.String()
	if !strings.Contains(out, "for mayor") {
		t.Errorf("stdout missing 'for mayor': %q", out)
	}
	if strings.Contains(out, "for worker") {
		t.Errorf("stdout should not contain 'for worker': %q", out)
	}
	if strings.Contains(out, "a task") {
		t.Errorf("stdout should not contain 'a task': %q", out)
	}
	if strings.Contains(out, "already read") {
		t.Errorf("stdout should not contain 'already read': %q", out)
	}
	if !strings.Contains(out, "1 unread message(s)") {
		t.Errorf("stdout missing correct count:\n%s", out)
	}
}

// --- ga-q6ct: identity-resolution session-list cache ---

// countingMailIdentityListStore counts broad gc:session List calls (the same
// query the cmd_mail identity-resolution path issues) so tests can assert the
// per-command cache budget.
type countingMailIdentityListStore struct {
	beads.Store
	sessionListCalls int
}

func (s *countingMailIdentityListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label == session.LabelSession && len(query.Metadata) == 0 {
		s.sessionListCalls++
	}
	return s.Store.List(query)
}

func TestResolveLiveConfiguredNamedMailTargetCached_SharesCacheAcrossCalls(t *testing.T) {
	// Pin: when a single command invocation resolves multiple identity
	// candidates (or recipient + sender both), the broad gc:session
	// enumeration runs at most once via the shared cache.
	base := beads.NewMemStore()
	store := &countingMailIdentityListStore{Store: base}

	if _, err := base.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			namedSessionIdentityMetadata: "gascity/builder",
			"alias":                      "builder-1",
		},
	}); err != nil {
		t.Fatalf("Create session: %v", err)
	}

	cache := &mailIdentitySessionCache{}
	for _, id := range []string{"unmatched-a", "unmatched-b", "unmatched-c"} {
		if _, _, err := resolveLiveConfiguredNamedMailTargetCached(store, id, cache); err != nil {
			t.Fatalf("resolve(%q): %v", id, err)
		}
	}

	if store.sessionListCalls != 1 {
		t.Errorf("broad gc:session List calls = %d, want 1 (cache must dedupe across resolutions)", store.sessionListCalls)
	}
}

func TestResolveLiveConfiguredNamedMailTargetCached_NilCacheStillFetches(t *testing.T) {
	// Backward-compat: passing nil cache should still resolve correctly,
	// issuing a broad scan per call (the legacy behavior).
	base := beads.NewMemStore()
	store := &countingMailIdentityListStore{Store: base}

	for _, id := range []string{"a", "b"} {
		if _, _, err := resolveLiveConfiguredNamedMailTargetCached(store, id, nil); err != nil {
			t.Fatalf("resolve(%q): %v", id, err)
		}
	}

	if store.sessionListCalls != 2 {
		t.Errorf("broad gc:session List calls = %d, want 2 (no cache → per-call scan)", store.sessionListCalls)
	}
}

func TestListLiveSessionMailboxesCached_UsesCache(t *testing.T) {
	// Pin: listLiveSessionMailboxesCached + a sibling resolve call sharing
	// the same cache hit the store at most once for the broad enumeration.
	base := beads.NewMemStore()
	store := &countingMailIdentityListStore{Store: base}

	if _, err := base.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			namedSessionIdentityMetadata: "gascity/mayor",
			"alias":                      "mayor",
		},
	}); err != nil {
		t.Fatalf("Create session: %v", err)
	}

	cache := &mailIdentitySessionCache{}
	if _, err := listLiveSessionMailboxesCached(store, cache); err != nil {
		t.Fatalf("listLiveSessionMailboxesCached: %v", err)
	}
	if _, _, err := resolveLiveConfiguredNamedMailTargetCached(store, "no-match", cache); err != nil {
		t.Fatalf("resolveLiveConfiguredNamedMailTargetCached: %v", err)
	}

	if store.sessionListCalls != 1 {
		t.Errorf("broad gc:session List calls = %d, want 1 across listLiveSessionMailboxes + resolve sharing one cache", store.sessionListCalls)
	}
}

func TestResolveMailIdentityWithConfigCached_SharedCacheSurvivesFallbackMiss(t *testing.T) {
	// Pin: the shared cache must stay in effect even when identity resolution
	// misses every shortcut and falls back to the generic resolution path.
	base := beads.NewMemStore()
	store := &countingMailIdentityListStore{Store: base}

	if _, err := base.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			namedSessionIdentityMetadata: "gascity/worker",
			"alias":                      "worker",
		},
	}); err != nil {
		t.Fatalf("Create session: %v", err)
	}

	cache := &mailIdentitySessionCache{}
	if _, err := listLiveSessionMailboxesCached(store, cache); err != nil {
		t.Fatalf("listLiveSessionMailboxesCached: %v", err)
	}
	if _, err := resolveMailIdentityWithConfigCached("", nil, store, "no-match", cache); !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolveMailIdentityWithConfigCached(no-match) error = %v, want ErrSessionNotFound", err)
	}

	if store.sessionListCalls != 1 {
		t.Errorf("broad gc:session List calls = %d, want 1 across listLiveSessionMailboxes + fallback miss resolution", store.sessionListCalls)
	}
}
