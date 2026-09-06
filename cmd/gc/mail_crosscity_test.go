package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/session"
)

func crossCityTestConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "qlandia"},
		Mail: config.MailConfig{CrossCity: &config.MailCrossCityConfig{
			Cities: []string{"gastown", "westeros"},
		}},
		Rigs: []config.Rig{{Name: "qcore", Path: "rigs/qcore"}},
	}
}

// A foreign-city recipient resolves from the roster alone. The nil session
// store makes the guard structural: a regression that consults the
// session store cannot pass.
func TestResolveMailRecipientIdentityCrossCityForeignWithoutSessionStore(t *testing.T) {
	got, err := resolveMailRecipientIdentity("", crossCityTestConfig(), nil, "gastown/mayor")
	if err != nil {
		t.Fatalf("resolveMailRecipientIdentity: %v", err)
	}
	if got != "gastown/mayor" {
		t.Errorf("got %q, want canonical %q", got, "gastown/mayor")
	}
}

func TestResolveMailRecipientIdentityCrossCityLocalStripHuman(t *testing.T) {
	got, err := resolveMailRecipientIdentity("", crossCityTestConfig(), nil, "qlandia/human")
	if err != nil {
		t.Fatalf("resolveMailRecipientIdentity: %v", err)
	}
	if got != "human" {
		t.Errorf("got %q, want %q", got, "human")
	}
}

// <local city>/<addr> and <addr> are one mailbox: both spellings resolve to
// the same canonical identity.
func TestResolveMailRecipientIdentityCrossCityLocalQualifiedAliasIsOneMailbox(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "mayor",
			"session_name": "mayor-session",
		},
	}); err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	cfg := crossCityTestConfig()
	bare, err := resolveMailRecipientIdentity("", cfg, store, "mayor")
	if err != nil {
		t.Fatalf("resolve bare: %v", err)
	}
	qualified, err := resolveMailRecipientIdentity("", cfg, store, "qlandia/mayor")
	if err != nil {
		t.Fatalf("resolve qualified: %v", err)
	}
	if bare != qualified {
		t.Errorf("qualified %q and bare %q must resolve to one mailbox", qualified, bare)
	}
}

// An address whose first segment names no rig and no roster city refuses as a
// typed unknown-city error — never as a session lookup failure; the two
// failures must never be spelled the same.
func TestResolveMailRecipientIdentityUnknownCityTypedError(t *testing.T) {
	_, err := resolveMailRecipientIdentity("", crossCityTestConfig(), nil, "gastwn/mayor")
	if err == nil {
		t.Fatal("expected unknown-city error")
	}
	var unknownCity *mail.UnknownCityError
	if !errors.As(err, &unknownCity) {
		t.Fatalf("err = %v (%T), want *mail.UnknownCityError", err, err)
	}
	if unknownCity.City != "gastwn" {
		t.Errorf("City = %q, want %q", unknownCity.City, "gastwn")
	}
	if errors.Is(err, session.ErrSessionNotFound) {
		t.Errorf("unknown-city refusal must not be a session-not-found error")
	}
}

// A rig-qualified name keeps today's failure shape even when the roster is
// active: the rig segment is a local scope, not a candidate city.
func TestResolveMailRecipientIdentityRigSegmentKeepsTodayError(t *testing.T) {
	_, err := resolveMailRecipientIdentity("", crossCityTestConfig(), nil, "qcore/nobody")
	if err == nil {
		t.Fatal("expected resolution failure")
	}
	var unknownCity *mail.UnknownCityError
	if errors.As(err, &unknownCity) {
		t.Errorf("rig-qualified failure must not be an unknown-city error: %v", err)
	}
}

// Without [mail.crosscity], a city-shaped recipient keeps today's behavior.
func TestResolveMailRecipientIdentityNoRosterUnchanged(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "qlandia"}}
	_, err := resolveMailRecipientIdentity("", cfg, nil, "gastown/mayor")
	if err == nil {
		t.Fatal("expected resolution failure without a roster")
	}
	var unknownCity *mail.UnknownCityError
	if errors.As(err, &unknownCity) {
		t.Errorf("no roster: failure must not be an unknown-city error: %v", err)
	}
}

// Read-side: resolving a local mail target expands its recipients with the
// city-qualified alias, so delivery addressed either way is one read.
func TestResolveMailTargetsCrossCityExpandsQualifiedAliases(t *testing.T) {
	target, err := resolveMailTargetsWithConfig("", crossCityTestConfig(), nil, "human")
	if err != nil {
		t.Fatalf("resolveMailTargetsWithConfig: %v", err)
	}
	if target.display != "human" {
		t.Errorf("display = %q, want %q", target.display, "human")
	}
	want := map[string]bool{"human": false, "qlandia/human": false}
	for _, r := range target.recipients {
		if _, ok := want[r]; ok {
			want[r] = true
		}
	}
	for addr, found := range want {
		if !found {
			t.Errorf("recipients %v must include %q", target.recipients, addr)
		}
	}
}

// Read-side: a foreign-city mailbox reads open-world, with no session lookup.
func TestResolveMailTargetsCrossCityForeignReadsOpenWorld(t *testing.T) {
	target, err := resolveMailTargetsWithConfig("", crossCityTestConfig(), nil, "gastown/mayor")
	if err != nil {
		t.Fatalf("resolveMailTargetsWithConfig: %v", err)
	}
	if target.display != "gastown/mayor" {
		t.Errorf("display = %q, want %q", target.display, "gastown/mayor")
	}
	if len(target.recipients) != 1 || target.recipients[0] != "gastown/mayor" {
		t.Errorf("recipients = %v, want [gastown/mayor]", target.recipients)
	}
}

func writeCrossCityTestCity(t *testing.T) string {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_MAIL", "")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_AGENT", "")
	cityPath := t.TempDir()
	cityTOML := `
[workspace]
name = "qlandia"

[mail.crosscity]
cities = ["gastown", "westeros"]
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", cityPath)
	return cityPath
}

func findMessageBead(t *testing.T, cityPath string) (beads.Bead, bool) {
	t.Helper()
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	all, err := store.List(beads.ListQuery{Type: "message", Status: "open", TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("List messages: %v", err)
	}
	for _, b := range all {
		if b.Type == "message" {
			return b, true
		}
	}
	return beads.Bead{}, false
}

// A fresh send to a peer city succeeds with no session for the recipient, and
// the stored sender is city-qualified so a plain reply crosses back.
func TestCmdMailSendCrossCityForeignRecipient(t *testing.T) {
	cityPath := writeCrossCityTestCity(t)

	var stdout, stderr bytes.Buffer
	code := cmdMailSend(nil, false, false, "human", "gastown/mayor", "cutover status", "leg is green", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdMailSend = %d, want 0; stderr=%s", code, stderr.String())
	}
	msg, found := findMessageBead(t, cityPath)
	if !found {
		t.Fatal("message bead not found")
	}
	if msg.Assignee != "gastown/mayor" {
		t.Errorf("Assignee = %q, want %q", msg.Assignee, "gastown/mayor")
	}
	if msg.From != "qlandia/human" {
		t.Errorf("From = %q, want city-qualified %q", msg.From, "qlandia/human")
	}
}

// --notify on a foreign recipient is a typed refusal at the sender, before
// any write: the recipient's wake belongs to its own city's sweep.
func TestCmdMailSendCrossCityNotifyRefused(t *testing.T) {
	cityPath := writeCrossCityTestCity(t)

	var stdout, stderr bytes.Buffer
	code := cmdMailSend(nil, true, false, "human", "gastown/mayor", "s", "b", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("cmdMailSend = %d, want 1; stdout=%s", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "--notify") || !strings.Contains(stderr.String(), "gastown") {
		t.Errorf("stderr = %q, want typed cross-city --notify refusal naming the city", stderr.String())
	}
	if _, found := findMessageBead(t, cityPath); found {
		t.Error("refusal must happen before the message is written")
	}
}

// --from carrying a foreign city segment is accepted verbatim: the sender's
// identity can only be checked by its own city.
func TestCmdMailSendCrossCityForeignFromAccepted(t *testing.T) {
	cityPath := writeCrossCityTestCity(t)

	var stdout, stderr bytes.Buffer
	code := cmdMailSend(nil, false, false, "westeros/mayor", "human", "relayed", "body", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdMailSend = %d, want 0; stderr=%s", code, stderr.String())
	}
	msg, found := findMessageBead(t, cityPath)
	if !found {
		t.Fatal("message bead not found")
	}
	if msg.From != "westeros/mayor" {
		t.Errorf("From = %q, want verbatim %q", msg.From, "westeros/mayor")
	}
}

// Delivery is a read on the operator's surface too: mail addressed to
// <local city>/human is served by the plain `gc mail inbox` human default.
// This drives the full command, not the resolver helper — the human
// fast-path in resolveMailTargetsForCommand must route through the roster.
func TestCmdMailInboxCrossCityServesCityQualifiedHumanDelivery(t *testing.T) {
	cityPath := writeCrossCityTestCity(t)
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	if _, err := beadmail.New(store).Send("gastown/mayor", "qlandia/human", "for the operator", "cutover done"); err != nil {
		t.Fatalf("seed Send: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdMailInbox(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdMailInbox = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "for the operator") {
		t.Errorf("inbox output %q must serve mail addressed to the city-qualified human form", stdout.String())
	}

	stdout.Reset()
	code = cmdMailCountWithJSON([]string{"human"}, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdMailCount = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "1") {
		t.Errorf("count output %q must include the city-qualified delivery", stdout.String())
	}
}

func seedForeignOriginMessage(t *testing.T, cityPath string) string {
	t.Helper()
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	seeded, err := beadmail.New(store).Send("gastown/mayor", "human", "cutover", "leg is green")
	if err != nil {
		t.Fatalf("seed Send: %v", err)
	}
	return seeded.ID
}

// A reply that crosses cities stores this city's sender city-qualified, so
// the far side's plain reply resolves back here.
func TestCmdMailReplyCrossCityQualifiesSender(t *testing.T) {
	cityPath := writeCrossCityTestCity(t)
	id := seedForeignOriginMessage(t, cityPath)

	var stdout, stderr bytes.Buffer
	code := cmdMailReply([]string{id, "received, thank you"}, "", "", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdMailReply = %d, want 0; stderr=%s", code, stderr.String())
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	all, err := store.List(beads.ListQuery{Type: "message", Status: "open", TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("List messages: %v", err)
	}
	var reply beads.Bead
	found := false
	for _, b := range all {
		if b.Type == "message" && b.ID != id {
			reply = b
			found = true
			break
		}
	}
	if !found {
		t.Fatal("reply bead not found")
	}
	if reply.Assignee != "gastown/mayor" {
		t.Errorf("reply Assignee = %q, want %q", reply.Assignee, "gastown/mayor")
	}
	if reply.From != "qlandia/human" {
		t.Errorf("reply From = %q, want city-qualified %q", reply.From, "qlandia/human")
	}
}

// --notify on a reply whose thread crosses cities is a typed refusal at the
// sender, before any write.
func TestCmdMailReplyCrossCityNotifyRefused(t *testing.T) {
	cityPath := writeCrossCityTestCity(t)
	id := seedForeignOriginMessage(t, cityPath)

	var stdout, stderr bytes.Buffer
	code := cmdMailReply([]string{id, "body"}, "", "", true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("cmdMailReply = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--notify") || !strings.Contains(stderr.String(), "gastown") {
		t.Errorf("stderr = %q, want typed cross-city --notify refusal naming the city", stderr.String())
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	all, err := store.List(beads.ListQuery{Type: "message", TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("List messages: %v", err)
	}
	count := 0
	for _, b := range all {
		if b.Type == "message" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("message count = %d, want 1 (refusal must happen before the reply is written)", count)
	}
}

// --from naming an unknown agent within the local city stays refused, with or
// without the city qualifier: the local city is the one place identity can
// and must be checked.
func TestCmdMailSendCrossCityLocalFromStillChecked(t *testing.T) {
	writeCrossCityTestCity(t)

	for _, from := range []string{"qlandia/nobody", "nobody"} {
		var stdout, stderr bytes.Buffer
		code := cmdMailSend(nil, false, false, from, "human", "s", "b", &stdout, &stderr)
		if code != 1 {
			t.Errorf("cmdMailSend --from %q = %d, want 1", from, code)
		}
		if !strings.Contains(stderr.String(), `invalid sender "nobody"`) {
			t.Errorf("--from %q stderr = %q, want the refused sender named", from, stderr.String())
		}
	}
}

// The invalid-sender refusal names the sender the caller gave. (Previously
// the message printed the zeroed resolution result: `invalid sender ""`.)
func TestCmdMailSendInvalidFromNamesTheGivenSender(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_MAIL", "")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_AGENT", "")
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"plain-city\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", cityPath)

	var stdout, stderr bytes.Buffer
	code := cmdMailSend(nil, false, false, "nobody", "human", "s", "b", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("cmdMailSend = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), `invalid sender "nobody"`) {
		t.Errorf("stderr = %q, want the refused sender named", stderr.String())
	}
}

// With the roster enabled, a reply whose thread origin cannot be read fails
// closed: the cross-city rules (notify refusal, sender qualification) cannot
// be applied to an unverifiable thread.
func TestCmdMailReplyCrossCityFailsClosedWhenOriginUnreadable(t *testing.T) {
	writeCrossCityTestCity(t)
	t.Setenv("GC_MAIL", "fail")

	var stdout, stderr bytes.Buffer
	code := cmdMailReply([]string{"gc-999", "body"}, "", "", false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("cmdMailReply = %d, want 1; stdout=%s", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "thread origin") {
		t.Errorf("stderr = %q, want the fail-closed origin-verification refusal", stderr.String())
	}
}

// The JSON surface refuses cross-city --notify with a typed error code.
func TestCmdMailSendCrossCityNotifyRefusedJSON(t *testing.T) {
	writeCrossCityTestCity(t)

	var stdout, stderr bytes.Buffer
	code := cmdMailSendJSON(nil, true, false, "human", "gastown/mayor", "s", "b", true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("cmdMailSendJSON = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "cross_city_notify") {
		t.Errorf("json output %q must carry the cross_city_notify error code", stdout.String())
	}
}
