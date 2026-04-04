package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
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
func (countOnlyMailProvider) Delete(string) error                  { panic("unexpected Delete") }
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

func TestResolveMailTargetsForCommand_UsesStoreForFakeProviderHistoricalAlias(t *testing.T) {
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

	var stderr bytes.Buffer
	target, ok := resolveMailTargetsForCommand("mayor", &stderr, "gc mail inbox")
	if !ok {
		t.Fatal("resolveMailTargetsForCommand() = not ok, want ok")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if target.display != "sky" {
		t.Fatalf("display = %q, want sky", target.display)
	}
	want := []string{"sky", b.ID, "mayor"}
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

func TestConfiguredMailboxAddressResolvesCityUniqueBareNamedSession(t *testing.T) {
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

	address, ok := configuredMailboxAddress("witness")
	if !ok {
		t.Fatal("configuredMailboxAddress() = not ok, want ok")
	}
	if address != "demo/witness" {
		t.Fatalf("address = %q, want demo/witness", address)
	}
}

func TestResolveMailRecipientIdentity_TemplatePrefixCreatesFreshSession(t *testing.T) {
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

	address, err := resolveMailRecipientIdentity(t.TempDir(), cfg, store, "template:mayor")
	if err != nil {
		t.Fatalf("resolveMailRecipientIdentity(template:mayor): %v", err)
	}
	if address == "mayor" {
		t.Fatalf("address = %q, want fresh session mailbox identity", address)
	}

	all, err := store.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("session bead count = %d, want 1", len(all))
	}
	if all[0].Metadata["alias"] != "" {
		t.Fatalf("fresh template mailbox alias = %q, want empty", all[0].Metadata["alias"])
	}
}

// --- gc mail inbox ---

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
	code := doMailReply(mp, events.Discard, "gc-1", "bob", "RE: Hello", "reply body", false, &stdout, &stderr)
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
