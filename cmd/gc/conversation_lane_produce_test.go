package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The CLI's lane notices (design slice 7a): mail --notify, sling --nudge and
// the hook-claim continuation, under the three rules of
// conversation_lane_produce.go.

// noticesBy lists the lane notices principal proposed on the fixture's
// session for causation.
func (c *cliLaneCity) noticesBy(t *testing.T, principal, causation string) []opjournal.Op {
	t.Helper()
	var out []opjournal.Op
	for _, op := range c.ops(t) {
		if op.Executor == opjournal.ExecutorLane && op.Conversation == c.id && op.Producer.Principal == principal && op.Causation == causation {
			out = append(out, op)
		}
	}
	return out
}

// noticeBy is the one lane notice principal proposed for causation, which
// is under a minted key, not one derived from its cause (Rule 2).
func (c *cliLaneCity) noticeBy(t *testing.T, principal, causation string) opjournal.Op {
	t.Helper()
	ops := c.noticesBy(t, principal, causation)
	if len(ops) != 1 {
		t.Fatalf("notices from %s caused by %s = %+v, want one", principal, causation, ops)
	}
	assertMintedNoticeKey(t, ops[0])
	return ops[0]
}

// assertMintedNoticeKey checks op's client key was minted: it names neither
// the notice's cause nor a cause-key prefix.
func assertMintedNoticeKey(t *testing.T, op opjournal.Op) {
	t.Helper()
	if op.ClientKey == "" || strings.Contains(op.ClientKey, op.Causation) || strings.Contains(op.ClientKey, ":") {
		t.Fatalf("op %s key %q, want a minted key that names no cause", op.ID, op.ClientKey)
	}
}

// assertLaneNotice checks op is a lane notice from principal, of policy
// wake, whose delivered text is golden and names no identifier.
func assertLaneNotice(t *testing.T, op opjournal.Op, principal string, wake opjournal.Wake, golden string) {
	t.Helper()
	m := op.Policy.Message
	if op.Executor != opjournal.ExecutorLane || op.Producer.Principal != principal || m.Role != opjournal.RoleNotice ||
		m.When != opjournal.WhenBoundary || m.Wake != wake {
		t.Fatalf("op = %+v, want a lane notice from %s waking %s", op, principal, wake)
	}
	text, err := opjournal.RenderDelivery([]opjournal.Op{op}, opjournal.TextReminder, false)
	if err != nil {
		t.Fatal(err)
	}
	if text != golden {
		t.Fatalf("delivered text = %q, want %q", text, golden)
	}
	for _, id := range []string{op.ID, op.ClientKey, op.Conversation, op.Causation} {
		if id != "" && strings.Contains(text, id) {
			t.Fatalf("delivered text %q carries identifier %q", text, id)
		}
	}
}

// reminderGolden is the delivered text of one notice labeled label.
func reminderGolden(label, text string) string {
	return "<system-reminder>\nYou have a deferred reminder that was queued until a safe boundary:\n\n- [" + label + "] " + text + "\n\nHandle them after this turn.\n</system-reminder>\n"
}

func TestMailNotifyUnderLaneProposesANotice(t *testing.T) {
	c := newCLILaneCity(t)
	id, err := sendMailNotifyWithOp(c.target, c.store, c.sp, "alice", "msg-1")
	if err != nil {
		t.Fatalf("notify: %v", err)
	}
	op := c.noticeBy(t, mailNoticePrincipal, "msg-1")
	if id != op.ID || op.Status == opjournal.StatusClosed {
		t.Fatalf("op %s = %+v, want an open notice caused by msg-1", id, op)
	}
	assertLaneNotice(t, op, mailNoticePrincipal, opjournal.WakeIfNeeded, reminderGolden("mail", "You have mail from alice"))
	if n := len(c.nudges()); n != 0 || c.queuedItems(t) != 0 {
		t.Fatalf("legacy delivery ran: %d nudges, %d queued", n, c.queuedItems(t))
	}
	if len(c.rings) != 1 || c.rings[0] != c.id+":"+op.ID {
		t.Fatalf("rings = %v, want one for the notice", c.rings)
	}
}

// Reading or archiving a message withdraws its open lane notices.
func TestMailReadCancelsItsNotice(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(p mail.Provider, id string) error
	}{
		{"read", func(p mail.Provider, id string) error { _, err := p.Read(id); return err }},
		{"mark read", func(p mail.Provider, id string) error { return p.MarkRead(id) }},
		{"archive", func(p mail.Provider, id string) error { return p.Archive(id) }},
		{"archive many", func(p mail.Provider, id string) error { _, err := p.ArchiveMany([]string{id}); return err }},
		{"delete", func(p mail.Provider, id string) error { return p.Delete(id) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCLILaneCity(t)
			inner := mail.NewFake()
			msg, err := inner.Send("alice", c.target.agentKey(), "hi", "body")
			if err != nil {
				t.Fatal(err)
			}
			opID, err := sendMailNotifyWithOp(c.target, c.store, c.sp, "alice", msg.ID)
			if err != nil {
				t.Fatal(err)
			}
			var warn bytes.Buffer
			p := withMailNoticeWithdrawal(inner, c.store, c.cfg, &warn)
			if err := tc.do(p, msg.ID); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			op := c.get(t, c.laneStore(t), opID)
			if op.Status != opjournal.StatusClosed || op.Outcome.Arm != opjournal.ArmCanceled {
				t.Fatalf("notice after %s = %s %+v, want it canceled; warnings %s", tc.name, op.Status, op.Outcome, warn.String())
			}
		})
	}
	// Without message listed the provider is not wrapped.
	c := newCLILaneCity(t)
	c.cfg.Ops.Authoritative = nil
	inner := mail.NewFake()
	if p := withMailNoticeWithdrawal(inner, c.store, c.cfg, nil); p != mail.Provider(inner) {
		t.Fatalf("provider wrapped with message unlisted: %T", p)
	}
}

// The city's bead mail provider keeps every capability its readers ask for
// (the session inbox, the handoff archive) under the withdrawal, and each of
// its reads and archives withdraws the message's open lane notices.
func TestBeadMailReadCancelsItsNotice(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(p mail.Provider, id string) error
	}{
		{"read", func(p mail.Provider, id string) error { _, err := p.Read(id); return err }},
		{"mark read", func(p mail.Provider, id string) error { return p.MarkRead(id) }},
		{"archive", func(p mail.Provider, id string) error { return p.Archive(id) }},
		{"archive many", func(p mail.Provider, id string) error { _, err := p.ArchiveMany([]string{id}); return err }},
		{"archive matching", func(p mail.Provider, _ string) error {
			_, _, err := p.(*beadmail.Provider).ArchiveMatching(beadmail.ArchiveFilter{From: "alice", IncludeRead: true})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCLILaneCity(t)
			inner := beadmail.New(c.store)
			msg, err := inner.Send("alice", c.target.agentKey(), "hi", "body")
			if err != nil {
				t.Fatal(err)
			}
			opID, err := sendMailNotifyWithOp(c.target, c.store, c.sp, "alice", msg.ID)
			if err != nil {
				t.Fatal(err)
			}
			var warn bytes.Buffer
			p := withMailNoticeWithdrawal(inner, c.store, c.cfg, &warn)
			if _, reads := p.(beadmail.SessionInboxReader); !reads {
				t.Fatalf("the withdrawal hides beadmail.SessionInboxReader (%T)", p)
			}
			if _, archives := p.(inboxAutoHandoffArchiver); !archives {
				t.Fatalf("the withdrawal hides the handoff archive (%T)", p)
			}
			if err := tc.do(p, msg.ID); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			op := c.get(t, c.laneStore(t), opID)
			if op.Status != opjournal.StatusClosed || op.Outcome.Arm != opjournal.ArmCanceled {
				t.Fatalf("notice after %s = %s %+v, want it canceled; warnings %s", tc.name, op.Status, op.Outcome, warn.String())
			}
		})
	}
}

// With message listed, an injected auto-handoff is still archived after its
// injection, so the next prompt's mail check does not inject it again.
func TestMailNoticeWithdrawalKeepsTheHandoffArchive(t *testing.T) {
	for _, listed := range []bool{false, true} {
		store := beads.NewMemStore()
		cfg := &config.City{}
		if listed {
			cfg.Ops.Authoritative = []string{"message"}
		}
		mp := withMailNoticeWithdrawal(beadmail.New(store), store, cfg, io.Discard)
		auto, err := store.Create(beads.Bead{
			Title: "context cycle", Type: "message", Assignee: "mayor", From: "mayor",
			Labels: []string{mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel},
		})
		if err != nil {
			t.Fatal(err)
		}
		var first, second bytes.Buffer
		if code := doMailCheck(mp, "mayor", true, &first, io.Discard); code != 0 {
			t.Fatalf("message listed %v: first mail check = %d", listed, code)
		}
		if !bytes.Contains(first.Bytes(), []byte(auto.ID)) {
			t.Fatalf("message listed %v: the handoff %s was not injected: %s", listed, auto.ID, first.String())
		}
		_ = doMailCheck(mp, "mayor", true, &second, io.Discard)
		if b, _ := store.Get(auto.ID); b.Status != "closed" || bytes.Contains(second.Bytes(), []byte(auto.ID)) {
			t.Fatalf("message listed %v: handoff %s is %s after its injection and injected again: %v", listed, auto.ID, b.Status, bytes.Contains(second.Bytes(), []byte(auto.ID)))
		}
	}
}

// cityOnDisk writes the fixture's city.toml, with message listed in [ops]
// authoritative, for the paths that load the city's config from disk, and
// points GC_CITY at it.
func (c *cliLaneCity) cityOnDisk(t *testing.T) {
	t.Helper()
	cityTOML := "[workspace]\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n\n[ops]\nauthoritative = [\"message\"]\n"
	if err := os.WriteFile(filepath.Join(c.dir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", c.dir)
	resetCLIStorageRoutes(t)
}

// mailWithNotice sends the fixture's session a message whose lane notice is
// open, and returns the message and its notice's op id.
func (c *cliLaneCity) mailWithNotice(t *testing.T, labels ...string) (beads.Bead, string) {
	t.Helper()
	msg, err := c.store.Create(beads.Bead{Title: "hi", Type: "message", Assignee: c.id, From: "alice", Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	opID, err := sendMailNotifyWithOp(c.target, c.store, c.sp, "alice", msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if op := c.get(t, c.laneStore(t), opID); op.Status == opjournal.StatusClosed {
		t.Fatalf("notice %s closed before the message was read: %+v", opID, op.Outcome)
	}
	return msg, opID
}

// assertWithdrawn checks the notice opID was canceled.
func (c *cliLaneCity) assertWithdrawn(t *testing.T, opID, after string, warnings *bytes.Buffer) {
	t.Helper()
	if op := c.get(t, c.laneStore(t), opID); op.Status != opjournal.StatusClosed || op.Outcome.Arm != opjournal.ArmCanceled {
		t.Fatalf("notice after %s = %s %+v, want it canceled; warnings %q", after, op.Status, op.Outcome, warnings.String())
	}
}

// Each production builder of the city's mail provider registers the
// withdrawal: a read through the CLI's (openCityMailProvider, behind gc mail
// read, mark-read, archive, delete and check) or the controller's
// (newCityMailProvider, behind the API's mail reads) cancels the message's
// open lane notice.
func TestCityMailProvidersWithdrawAReadMessagesNotice(t *testing.T) {
	t.Run("openCityMailProvider", func(t *testing.T) {
		clearGCEnv(t)
		c := newCLILaneCity(t)
		c.cityOnDisk(t)
		prevFactory := openStoreFactoryForCity
		openStoreFactoryForCity = func(context.Context, beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
			return beads.StoreOpenResult{Store: c.store}, nil
		}
		t.Cleanup(func() { openStoreFactoryForCity = prevFactory })
		msg, opID := c.mailWithNotice(t)
		var stderr bytes.Buffer
		p, code := openCityMailProvider(&stderr, "gc mail read")
		if code != 0 {
			t.Fatalf("openCityMailProvider = %d; stderr %s", code, stderr.String())
		}
		if _, err := p.Read(msg.ID); err != nil {
			t.Fatalf("read: %v", err)
		}
		c.assertWithdrawn(t, opID, "a read", &stderr)
	})
	t.Run("newCityMailProvider", func(t *testing.T) {
		clearGCEnv(t)
		c := newCLILaneCity(t)
		msg, opID := c.mailWithNotice(t)
		p := newCityMailProvider(nil, c.store, c.cfg, c.dir, nil)
		if _, err := p.Read(msg.ID); err != nil {
			t.Fatalf("read: %v", err)
		}
		c.assertWithdrawn(t, opID, "a read", &bytes.Buffer{})
	})
}

// SessionStart (sessionStartMailInjection, as gc prime --hook runs it)
// archives the auto-handoff it injected through a bead mail provider of its
// own; with message listed, that archive withdraws the handoff's open lane
// notice, as gc mail check --inject's does.
func TestSessionStartHandoffArchiveWithdrawsItsNotice(t *testing.T) {
	clearGCEnv(t)
	c := newCLILaneCity(t)
	c.cityOnDisk(t)
	t.Setenv("GC_SESSION_ID", c.id)
	handoff, opID := c.mailWithNotice(t, mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel)
	live := c.rowInfo(t)
	var stderr bytes.Buffer
	startMail := sessionStartMailInjection(c.store, &live, c.dir, &stderr)
	if !startMail.autoHandoffIDs[handoff.ID] || startMail.autoHandoff.afterDelivery == nil {
		t.Fatalf("SessionStart did not inject handoff %s (ids %v); stderr %s", handoff.ID, startMail.autoHandoffIDs, stderr.String())
	}
	startMail.autoHandoff.afterDelivery()
	if b, err := c.store.Get(handoff.ID); err != nil || b.Status != "closed" {
		t.Fatalf("handoff after its injection = %+v, %v; want it archived; stderr %s", b, err, stderr.String())
	}
	c.assertWithdrawn(t, opID, "the SessionStart archive", &stderr)
}

func TestSlingNudgeUnderLaneProposesANotice(t *testing.T) {
	c := newCLILaneCity(t)
	var stdout, stderr bytes.Buffer
	deliverSlingNudge(c.target, c.sp, c.store, c.store, c.dir, "gc-bead-1", &stdout, &stderr)
	if !strings.Contains(stdout.String(), "in the conversation lane") || stderr.Len() != 0 {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
	op := c.noticeBy(t, slingNoticePrincipal, "gc-bead-1")
	assertLaneNotice(t, op, slingNoticePrincipal, opjournal.WakeIfNeeded, reminderGolden("sling", slingNudgeText))
	if n := len(c.nudges()); n != 0 || c.queuedItems(t) != 0 {
		t.Fatalf("legacy delivery ran: %d nudges, %d queued", n, c.queuedItems(t))
	}
}

// assertOneNoticePerEvent checks principal proposed want notices for
// causation, each its own op under its own minted key (Rule 2), open is how
// many are still open, and nothing took the legacy path.
func (c *cliLaneCity) assertOneNoticePerEvent(t *testing.T, principal, causation string, want, open int) []opjournal.Op {
	t.Helper()
	ops := c.noticesBy(t, principal, causation)
	if len(ops) != want {
		t.Fatalf("notices = %+v, want %d: one per event", ops, want)
	}
	keys := map[string]bool{}
	gotOpen := 0
	for _, op := range ops {
		assertMintedNoticeKey(t, op)
		if keys[op.ClientKey] {
			t.Fatalf("two notices under the key %q", op.ClientKey)
		}
		keys[op.ClientKey] = true
		if op.Status != opjournal.StatusClosed {
			gotOpen++
		}
	}
	if gotOpen != open {
		t.Fatalf("open notices = %d of %+v, want %d", gotOpen, ops, open)
	}
	if n := len(c.nudges()); n != 0 || c.queuedItems(t) != 0 {
		t.Fatalf("legacy delivery ran: %d nudges, %d queued", n, c.queuedItems(t))
	}
	return ops
}

// Every gc sling --nudge of a bead is its own notice (Rule 2): a re-sling
// while the first notice is open proposes a second rather than coalescing
// into it, and one after a notice closed proposes another rather than
// answering from the closed op. The lane delivers the open ones together.
func TestSlingNudgeUnderLaneEachSlingProposesOneNotice(t *testing.T) {
	c := newCLILaneCity(t)
	sling := func() {
		t.Helper()
		var stdout, stderr bytes.Buffer
		deliverSlingNudge(c.target, c.sp, c.store, c.store, c.dir, "gc-bead-1", &stdout, &stderr)
		if !strings.Contains(stdout.String(), "in the conversation lane") || stderr.Len() != 0 {
			t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
		}
	}
	sling()
	sling()
	ops := c.assertOneNoticePerEvent(t, slingNoticePrincipal, "gc-bead-1", 2, 2)
	if _, err := c.laneStore(t).Close(ops[0].ID, opjournal.Outcome{Arm: opjournal.ArmSucceeded}, "lane:test"); err != nil {
		t.Fatalf("close: %v", err)
	}
	sling()
	c.assertOneNoticePerEvent(t, slingNoticePrincipal, "gc-bead-1", 3, 2)
	if len(c.rings) != 3 {
		t.Fatalf("rings = %v, want one per notice", c.rings)
	}
}

// Every claim of a workflow root is its own continuation notice (Rule 2),
// scoped to the session's generation at the claim: a second claim while the
// first notice is open, one after it closed, and one in a later generation
// each propose a notice, never the legacy queue.
func TestHookClaimContinuationUnderLaneEachClaimProposesOneNotice(t *testing.T) {
	c := newCLILaneCity(t)
	claim := func(target nudgeTarget) {
		t.Helper()
		if handled, err := continuationLaneNotice(target, c.store, c.sp, "gc-root-1"); !handled || err != nil {
			t.Fatalf("continuation = %v, %v; want proposed", handled, err)
		}
	}
	claim(c.target)
	claim(c.target)
	ops := c.assertOneNoticePerEvent(t, hookClaimContinuationPrincipal, "gc-root-1", 2, 2)
	if _, err := c.laneStore(t).Close(ops[0].ID, opjournal.Outcome{Arm: opjournal.ArmSucceeded}, "lane:test"); err != nil {
		t.Fatalf("close: %v", err)
	}
	claim(c.target)
	c.assertOneNoticePerEvent(t, hookClaimContinuationPrincipal, "gc-root-1", 3, 2)
	if err := c.store.SetMetadata(c.id, "generation", "2"); err != nil {
		t.Fatalf("bump generation: %v", err)
	}
	claim(resolveNudgeTargetFromSessionInfo(c.dir, c.cfg, c.rowInfo(t)))
	gens := map[int64]int{}
	for _, op := range c.assertOneNoticePerEvent(t, hookClaimContinuationPrincipal, "gc-root-1", 4, 3) {
		if op.Scope.Kind != opjournal.ScopeSession {
			t.Fatalf("op %+v, want scoped to the session", op)
		}
		gens[op.Scope.Generation]++
	}
	if gens[1] != 3 || gens[2] != 1 {
		t.Fatalf("notices per generation = %v, want 3 in generation 1 and 1 in generation 2", gens)
	}
}

// Every mail notify is its own notice (Rule 2), caused by its message.
func TestMailNotifyUnderLaneEachNotifyProposesOneNotice(t *testing.T) {
	c := newCLILaneCity(t)
	first, err := sendMailNotifyWithOp(c.target, c.store, c.sp, "alice", "msg-1")
	if err != nil {
		t.Fatalf("notify: %v", err)
	}
	if _, err := c.laneStore(t).Close(first, opjournal.Outcome{Arm: opjournal.ArmSucceeded}, "lane:test"); err != nil {
		t.Fatalf("close: %v", err)
	}
	second, err := sendMailNotifyWithOp(c.target, c.store, c.sp, "alice", "msg-1")
	if err != nil || second == "" || second == first {
		t.Fatalf("second notify = %q, %v; want a new notice, not %s", second, err, first)
	}
	c.assertOneNoticePerEvent(t, mailNoticePrincipal, "msg-1", 2, 1)
}

func TestHookClaimContinuationUnderLaneProposesANotice(t *testing.T) {
	c := newCLILaneCity(t)
	if handled, err := continuationLaneNotice(c.target, c.store, c.sp, "gc-root-1"); !handled || err != nil {
		t.Fatalf("continuationLaneNotice = %v, %v; want proposed", handled, err)
	}
	op := c.noticeBy(t, hookClaimContinuationPrincipal, "gc-root-1")
	if op.Scope.Kind != opjournal.ScopeSession || op.Scope.Generation != 1 {
		t.Fatalf("op = %+v, want scoped to the generation", op)
	}
	assertLaneNotice(t, op, hookClaimContinuationPrincipal, opjournal.WakeLiveOnly, reminderGolden("hook-claim-continuation", hookClaimContinuationText))
	if n := len(c.nudges()); n != 0 || c.queuedItems(t) != 0 {
		t.Fatalf("legacy delivery ran: %d nudges, %d queued", n, c.queuedItems(t))
	}
}

// With message unlisted every CLI producer keeps its legacy path, writes no
// lane op and reads no session row for the lane: a nil city store is not
// refused.
func TestCLIProducersWithoutLaneKeepLegacy(t *testing.T) {
	c := newCLILaneCity(t)
	c.cfg.Ops.Authoritative = nil
	c.target.cfg = c.cfg
	if prod, err := routeCLILaneNotice(cliLaneNoticeRoute{
		target: c.target, sp: c.sp,
		req: cliLaneNotice(mailNoticePrincipal, "m", "x", opjournal.WakeIfNeeded),
	}); prod.handled || err != nil {
		t.Fatalf("routeCLILaneNotice with message unlisted = %+v, %v; want unhandled", prod, err)
	}
	var stdout, stderr bytes.Buffer
	deliverSlingNudge(c.target, c.sp, c.store, nil, c.dir, "gc-bead-1", &stdout, &stderr)
	if strings.Contains(stdout.String(), "conversation lane") {
		t.Fatalf("sling took the lane: %q", stdout.String())
	}
	if handled, err := continuationLaneNotice(c.target, c.store, c.sp, "gc-root-1"); handled || err != nil {
		t.Fatalf("the continuation took the lane: %v, %v", handled, err)
	}
	for _, op := range c.ops(t) {
		if op.Executor == opjournal.ExecutorLane {
			t.Fatalf("lane op %+v written with message unlisted", op)
		}
	}
}

// laneProposeFailStore refuses to create any op row: a proposal that fails
// before its row exists.
type laneProposeFailStore struct{ laneTestStore }

func (s laneProposeFailStore) Create(b beads.Bead) (beads.Bead, error) {
	if b.Type == opjournal.BeadType {
		return beads.Bead{}, errors.New("store hiccup")
	}
	return s.laneTestStore.Create(b)
}

// cliLaneRefusalCases are the ways a CLI lane notice's op cannot be written
// once a lane serves its session: the store the producer is handed, over the
// fixture's, and the cause its refusal names.
var cliLaneRefusalCases = []struct {
	name  string
	store func(c *cliLaneCity, base *laneGetFailStore) beads.Store
	cause string
}{
	{name: "the op journal cannot open", store: func(_ *cliLaneCity, base *laneGetFailStore) beads.Store {
		return noBatchStore{base}
	}, cause: "opening lane store"},
	{name: "the session row cannot be read", store: func(c *cliLaneCity, base *laneGetFailStore) beads.Store {
		base.failID, base.fails = c.id, 1
		return base
	}, cause: "reading session "},
	{name: "the proposal fails", store: func(_ *cliLaneCity, base *laneGetFailStore) beads.Store {
		return laneProposeFailStore{base.laneTestStore}
	}, cause: "store hiccup"},
}

// Under lane authority a notice whose op cannot be written is refused and
// reported, as gc session nudge's request is (TestSessionNudgeLaneRefusesInRun):
// no CLI producer falls back to its legacy delivery or queue, and nothing is
// written or rung.
func TestCLILaneNoticeRefusalsNeverFallBack(t *testing.T) {
	producers := []struct {
		name string
		// run runs the producer over store and returns its report.
		run func(t *testing.T, c *cliLaneCity, store beads.Store) string
	}{
		{name: "sling", run: func(t *testing.T, c *cliLaneCity, store beads.Store) string {
			var stdout, stderr bytes.Buffer
			deliverSlingNudge(c.target, c.sp, store, store, c.dir, "gc-bead-1", &stdout, &stderr)
			if stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), "warning: bead routed but nudge failed: op_journal_unavailable: ") {
				t.Fatalf("stdout %q, stderr %q; want only the refusal's warning", stdout.String(), stderr.String())
			}
			return stderr.String()
		}},
		{name: "mail notify", run: func(t *testing.T, c *cliLaneCity, store beads.Store) string {
			id, err := sendMailNotifyWithOp(c.target, store, c.sp, "alice", "msg-1")
			if !errors.Is(err, errCLILaneUnavailable) || id != "" {
				t.Fatalf("notify = %q, %v; want the refusal", id, err)
			}
			return err.Error()
		}},
		{name: "hook-claim continuation", run: func(t *testing.T, c *cliLaneCity, store beads.Store) string {
			handled, err := continuationLaneNotice(c.target, store, c.sp, "gc-root-1")
			if !handled || !errors.Is(err, errCLILaneUnavailable) {
				t.Fatalf("continuation = %v, %v; want handled with the refusal", handled, err)
			}
			return err.Error()
		}},
	}
	for _, p := range producers {
		for _, tc := range cliLaneRefusalCases {
			t.Run(p.name+"/"+tc.name, func(t *testing.T) {
				base := &laneGetFailStore{laneTestStore: newLaneTestStore()}
				c := newCLILaneCityOver(t, base)
				calls := len(c.nudges())
				if got := p.run(t, c, tc.store(c, base)); !strings.Contains(got, tc.cause) {
					t.Fatalf("refusal %q, want its cause %q", got, tc.cause)
				}
				base.fails = 0
				if ops := c.ops(t); len(ops) != 0 {
					t.Fatalf("op rows %+v, want none", ops)
				}
				if n := c.queuedItems(t); n != 0 {
					t.Fatalf("%d queue items, want none", n)
				}
				if n := len(c.nudges()); n != calls {
					t.Fatalf("%d deliveries, want none", n-calls)
				}
				if len(c.rings) != 0 {
					t.Fatalf("rings %v after a refusal", c.rings)
				}
			})
		}
	}
}

// The claim's continuation nudge through its production path
// (hookContinuationNudgeEnqueue, as writeHookClaimWorkResultForBead calls
// it) with message listed: the session a lane serves gets the root's
// notice, caused by the root, and no queue item. A notice the op journal
// cannot record, or whose nudge store or session provider does not open, is
// warned of on stderr and nothing is queued.
func TestHookContinuationNudgeEnqueueUnderLane(t *testing.T) {
	for _, tc := range []struct {
		name string
		// opened is the store the call opens, over the fixture's; unopened
		// opens none, as when the nudge store does not open.
		opened   func(laneTestStore) beads.Store
		unopened bool
		broken   bool
		warning  string
	}{
		{name: "proposes the root's notice", opened: func(s laneTestStore) beads.Store { return s }},
		{name: "the op journal cannot open", opened: func(s laneTestStore) beads.Store { return noBatchStore{s} }, warning: "opening lane store"},
		{name: "the session provider does not open", opened: func(s laneTestStore) beads.Store { return s }, broken: true, warning: "opening the session provider"},
		{name: "the nudge store does not open", unopened: true, warning: "the city store did not open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			t.Setenv("GC_BEADS", "file")
			cityDir := t.TempDir()
			cityTOML := "[workspace]\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n\n[ops]\nauthoritative = [\"message\"]\n"
			if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
				t.Fatalf("WriteFile(city.toml): %v", err)
			}
			t.Setenv("GC_CITY", cityDir)
			resetCLIStorageRoutes(t)
			if tc.broken {
				t.Setenv("GC_SESSION", "broken")
				prevBuild := buildSessionProviderByName
				buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
					return nil, errors.New("injected provider failure")
				}
				t.Cleanup(func() { buildSessionProviderByName = prevBuild })
			}
			store := newLaneTestStore()
			row, err := store.Create(beads.Bead{
				Title: "Session: worker", Type: session.BeadType, Status: "open", Labels: []string{session.LabelSession},
				Metadata: map[string]string{
					"session_name": "worker-session", "agent_name": "worker", "template": "worker",
					"state": string(session.StateActive), "generation": "1", "continuation_epoch": "1", "instance_token": "tok-1",
				},
			})
			if err != nil {
				t.Fatalf("creating the session row: %v", err)
			}
			prevOpen, prevPoller, prevRing := openOwnedNudgeBeadStore, startNudgePoller, cliLaneRing
			t.Cleanup(func() { openOwnedNudgeBeadStore, startNudgePoller, cliLaneRing = prevOpen, prevPoller, prevRing })
			openOwnedNudgeBeadStore = func(string) (beads.NudgesStore, beads.Store) {
				if tc.unopened {
					return beads.NudgesStore{}, nil
				}
				return beads.NudgesStore{Store: store}, tc.opened(store)
			}
			var pollers, rings []string
			startNudgePoller = func(_, _, sessionName string) error {
				pollers = append(pollers, sessionName)
				return nil
			}
			cliLaneRing = func(_, conv, opID string) (string, error) {
				rings = append(rings, conv+":"+opID)
				return "ok", nil
			}

			var stderr bytes.Buffer
			hookContinuationNudgeEnqueue("worker-session", "gc-root-1", &stderr)

			state, err := nudgequeue.LoadState(cityDir)
			if err != nil {
				t.Fatalf("LoadState: %v", err)
			}
			if len(state.Pending) != 0 || len(pollers) != 0 {
				t.Fatalf("queued %+v, pollers %v; want nothing queued", state.Pending, pollers)
			}
			ops := cliOpRows(t, store)
			if tc.warning != "" {
				got := stderr.String()
				if !strings.HasPrefix(got, "gc hook --claim: warning: the continuation nudge for gc-root-1 was not sent: op_journal_unavailable: ") || !strings.Contains(got, tc.warning) {
					t.Fatalf("stderr %q, want the refusal naming %q", got, tc.warning)
				}
				if len(ops) != 0 || len(rings) != 0 {
					t.Fatalf("op rows %+v, rings %v after a refusal", ops, rings)
				}
				return
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr %q", stderr.String())
			}
			if len(ops) != 1 || ops[0].Conversation != row.ID || ops[0].Executor != opjournal.ExecutorLane || ops[0].Producer.Principal != hookClaimContinuationPrincipal ||
				ops[0].Causation != "gc-root-1" || ops[0].Status == opjournal.StatusClosed {
				t.Fatalf("op rows %+v, want the root's open lane notice", ops)
			}
			assertMintedNoticeKey(t, ops[0])
			if len(rings) != 1 || rings[0] != row.ID+":"+ops[0].ID {
				t.Fatalf("rings %v, want one for the notice", rings)
			}
		})
	}
}

// gc sling --nudge through its production path (doSling → doSlingNudge →
// deliverSlingNudge) with message listed proposes the slung bead's notice,
// caused by the bead; a notice the op journal cannot record is warned of,
// and nothing is delivered or queued.
func TestDoSlingNudgeUnderLaneProposesTheBeadsNotice(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(map[bool]string{false: "proposes", true: "refused"}[refused], func(t *testing.T) {
			c := newCLILaneCity(t)
			if _, err := c.store.Create(beads.Bead{ID: "gc-work-1", Title: "work", Type: "task", Status: "open"}); err != nil {
				t.Fatalf("creating the bead: %v", err)
			}
			c.cfg.Workspace.Name = "test-city"
			prevPoller := startNudgePoller
			startNudgePoller = func(_, _, _ string) error { return nil }
			t.Cleanup(func() { startNudgePoller = prevPoller })
			runner := newFakeRunner()
			deps, stdout, stderr := testDeps(c.cfg, c.sp, runner.run)
			deps.CityPath = c.dir
			deps.Store = c.store
			if refused {
				deps.Store = noBatchStore{c.store}
			}
			opts := testOpts(config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}, "gc-work-1")
			opts.Nudge = true
			calls := len(c.nudges())
			if code := doSling(opts, deps, nil, stdout, stderr); code != 0 {
				t.Fatalf("doSling = %d; stderr %s", code, stderr.String())
			}
			if n := len(c.nudges()); n != calls || c.queuedItems(t) != 0 {
				t.Fatalf("legacy delivery ran: %d nudges, %d queued", n-calls, c.queuedItems(t))
			}
			if refused {
				if !strings.Contains(stderr.String(), "warning: bead routed but nudge failed: op_journal_unavailable: ") || len(c.ops(t)) != 0 {
					t.Fatalf("stderr %q, ops %+v; want the refusal and no op", stderr.String(), c.ops(t))
				}
				return
			}
			op := c.noticeBy(t, slingNoticePrincipal, "gc-work-1")
			if op.Status == opjournal.StatusClosed || !strings.Contains(stdout.String(), "in the conversation lane") {
				t.Fatalf("op %+v, stdout %q; want the bead's open notice", op, stdout.String())
			}
			assertLaneNotice(t, op, slingNoticePrincipal, opjournal.WakeIfNeeded, reminderGolden("sling", slingNudgeText))
		})
	}
}

// gc sling --nudge of a rig bead to a fixed agent: the rig's store holds the
// bead but no session row, since session rows live in the city store. With
// message listed the target resolves in the city store, as the pool branch's
// does (#2579), so the bead's notice lands in the sessions class the lane
// host reads, and nothing takes the legacy path into the session the lane
// serves. A city store that does not open is refused, as a notice the op
// journal cannot record is. With message unlisted the city store is never
// opened, and the nudge keeps its legacy path.
func TestDoSlingNudgeUnderLaneResolvesARigBeadsSessionInTheCityStore(t *testing.T) {
	for _, tc := range []struct {
		name    string
		listed  bool
		openErr error
	}{
		{name: "proposes", listed: true},
		{name: "the city store does not open", listed: true, openErr: errors.New("city store down")},
		{name: "message unlisted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCLILaneCity(t)
			if !tc.listed {
				c.cfg.Ops.Authoritative = nil
			}
			c.cfg.Workspace.Name = "test-city"
			writeSlingTestCity(t, c.dir, "[workspace]\nname = \"test-city\"\n")
			rig := &beads.MemStore{IDPrefix: "rg", HonorExplicitIDs: true}
			if _, err := rig.Create(beads.Bead{ID: "rg-work-1", Title: "work", Type: "task", Status: "open"}); err != nil {
				t.Fatalf("creating the bead: %v", err)
			}
			opened := 0
			prevOpen := slingOpenCityStore
			slingOpenCityStore = func(path string) (beads.Store, error) {
				opened++
				if path != c.dir {
					t.Fatalf("slingOpenCityStore(%q), want %q", path, c.dir)
				}
				if tc.openErr != nil {
					return nil, tc.openErr
				}
				return c.store, nil
			}
			t.Cleanup(func() { slingOpenCityStore = prevOpen })
			prevPoller := startNudgePoller
			startNudgePoller = func(_, _, _ string) error { return nil }
			t.Cleanup(func() { startNudgePoller = prevPoller })
			runner := newFakeRunner()
			deps, stdout, stderr := testDeps(c.cfg, c.sp, runner.run)
			deps.CityPath = c.dir
			deps.Store = rig
			opts := testOpts(config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}, "rg-work-1")
			opts.Nudge = true
			calls := len(c.nudges())
			if code := doSling(opts, deps, nil, stdout, stderr); code != 0 {
				t.Fatalf("doSling = %d; stderr %s", code, stderr.String())
			}
			if ops := cliOpRows(t, rig); len(ops) != 0 {
				t.Fatalf("op rows in the rig store %+v, want none", ops)
			}
			if !tc.listed {
				if opened != 0 || c.queuedItems(t) != 1 || len(c.ops(t)) != 0 {
					t.Fatalf("city store opened %d times, %d queued, ops %+v; want the legacy queue item alone", opened, c.queuedItems(t), c.ops(t))
				}
				return
			}
			if n := len(c.nudges()); n != calls || c.queuedItems(t) != 0 {
				t.Fatalf("legacy delivery ran: %d nudges, %d queued; stdout %q stderr %q", n-calls, c.queuedItems(t), stdout.String(), stderr.String())
			}
			if tc.openErr != nil {
				if !strings.Contains(stderr.String(), "warning: bead routed but nudge failed: op_journal_unavailable: ") ||
					!strings.Contains(stderr.String(), tc.openErr.Error()) || len(c.ops(t)) != 0 {
					t.Fatalf("stderr %q, ops %+v; want the refusal and no op", stderr.String(), c.ops(t))
				}
				return
			}
			if !strings.Contains(stdout.String(), "in the conversation lane") || stderr.Len() != 0 {
				t.Fatalf("stdout %q, stderr %q; want the notice proposed", stdout.String(), stderr.String())
			}
			op := c.noticeBy(t, slingNoticePrincipal, "rg-work-1")
			if op.Status == opjournal.StatusClosed {
				t.Fatalf("op %+v; want the bead's open notice", op)
			}
			assertLaneNotice(t, op, slingNoticePrincipal, opjournal.WakeIfNeeded, reminderGolden("sling", slingNudgeText))
		})
	}
}

// laneCausationFailStore fails the next fails keyed reads of ops by
// causation (opjournal.Store.OpenByCausation): a withdrawal that cannot list
// a message's notices.
type laneCausationFailStore struct {
	laneTestStore
	fails int
}

func (s *laneCausationFailStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.fails > 0 && q.Metadata["gc.op.causation"] != "" {
		s.fails--
		return nil, errors.New("store hiccup")
	}
	return s.laneTestStore.List(q)
}

// A withdrawal that fails after its message is archived is retried by the
// next archive, which finds the message already archived, through the bead
// mail provider's consumed hook and through the wrapper alike.
func TestMailArchiveRetriesAFailedWithdrawal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		inner func(store beads.Store) mail.Provider
	}{
		{"wrapped provider", func(beads.Store) mail.Provider { return mail.NewFake() }},
		{"bead mail", func(store beads.Store) mail.Provider { return beadmail.New(store) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &laneCausationFailStore{laneTestStore: newLaneTestStore()}
			c := newCLILaneCityOver(t, store)
			inner := tc.inner(c.store)
			msg, err := inner.Send("alice", c.target.agentKey(), "hi", "body")
			if err != nil {
				t.Fatal(err)
			}
			opID, err := sendMailNotifyWithOp(c.target, c.store, c.sp, "alice", msg.ID)
			if err != nil {
				t.Fatal(err)
			}
			var warn bytes.Buffer
			p := withMailNoticeWithdrawal(inner, c.store, c.cfg, &warn)
			store.fails = 1
			if err := p.Archive(msg.ID); err != nil {
				t.Fatalf("archive: %v", err)
			}
			if op := c.get(t, c.laneStore(t), opID); op.Status == opjournal.StatusClosed || !strings.Contains(warn.String(), "listing the notices of "+msg.ID) {
				t.Fatalf("notice after a failed withdrawal = %s, warnings %q; want it open and the failure warned of", op.Status, warn.String())
			}
			if err := p.Archive(msg.ID); !errors.Is(err, mail.ErrAlreadyArchived) {
				t.Fatalf("second archive = %v, want mail.ErrAlreadyArchived", err)
			}
			if op := c.get(t, c.laneStore(t), opID); op.Status != opjournal.StatusClosed || op.Outcome.Arm != opjournal.ArmCanceled {
				t.Fatalf("notice after the second archive = %s %+v, want it canceled; warnings %q", op.Status, op.Outcome, warn.String())
			}
		})
	}
}

// laneSessionListFailStore fails every list of session rows: a city store
// whose session lookup by name cannot answer.
type laneSessionListFailStore struct{ laneTestStore }

func (s laneSessionListFailStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Label == session.LabelSession || q.Type == session.BeadType || q.Metadata["session_name"] != "" || q.Metadata["alias"] != "" {
		return nil, errors.New("store hiccup")
	}
	return s.laneTestStore.List(q)
}

// Rule 1: routeCLILaneNotice reads the target's row in the city's store it
// is handed, by the target's session id and else by its session name. A
// target with no row there, and a session no lane serves, keep the legacy
// path, nothing written; a city store that did not open, and a lookup that
// cannot answer, are refusals, never taken for a missing row.
func TestRouteCLILaneNotice(t *testing.T) {
	notice := cliLaneNotice(slingNoticePrincipal, "gc-bead-1", slingNudgeText, opjournal.WakeIfNeeded)
	for _, tc := range []struct {
		name string
		// city is the city store handed over, over the fixture's; nil hands none.
		city func(c *cliLaneCity) beads.Store
		// target is the routed target, from the fixture's.
		target  func(c *cliLaneCity) nudgeTarget
		modify  func(t *testing.T, c *cliLaneCity)
		handled bool
		refusal string
	}{
		{name: "by the session id", handled: true},
		{name: "by the session name, with no id", target: func(c *cliLaneCity) nudgeTarget {
			return nudgeTarget{cityPath: c.dir, cfg: c.cfg, sessionName: c.target.sessionName}
		}, handled: true},
		{name: "an id the city store does not hold falls back to the name", target: func(c *cliLaneCity) nudgeTarget {
			return nudgeTarget{cityPath: c.dir, cfg: c.cfg, sessionID: "gc-elsewhere", sessionName: c.target.sessionName}
		}, handled: true},
		{name: "no row keeps the legacy path", target: func(c *cliLaneCity) nudgeTarget {
			return nudgeTarget{cityPath: c.dir, cfg: c.cfg, sessionName: "nobody-session"}
		}},
		{name: "a closed row keeps the legacy path", modify: func(t *testing.T, c *cliLaneCity) {
			t.Helper()
			if err := c.store.Close(c.id); err != nil {
				t.Fatalf("closing the row: %v", err)
			}
		}},
		{name: "a protocol-mode session no lane serves keeps the legacy path", modify: func(_ *testing.T, c *cliLaneCity) {
			c.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
		}},
		{name: "no city store is refused", city: func(*cliLaneCity) beads.Store { return nil }, handled: true, refusal: "the city store did not open"},
		{name: "a row read error is refused", city: func(c *cliLaneCity) beads.Store {
			return &laneGetFailStore{laneTestStore: c.store.(laneTestStore), failID: c.id, fails: 1}
		}, handled: true, refusal: "reading session "},
		{name: "a lookup by name that cannot answer is refused", city: func(c *cliLaneCity) beads.Store {
			return laneSessionListFailStore{c.store.(laneTestStore)}
		}, target: func(c *cliLaneCity) nudgeTarget {
			return nudgeTarget{cityPath: c.dir, cfg: c.cfg, sessionName: c.target.sessionName}
		}, handled: true, refusal: "store hiccup"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCLILaneCity(t)
			if tc.modify != nil {
				tc.modify(t, c)
			}
			city := c.store
			if tc.city != nil {
				city = tc.city(c)
			}
			target := c.target
			if tc.target != nil {
				target = tc.target(c)
			}
			prod, err := routeCLILaneNotice(cliLaneNoticeRoute{target: target, city: city, sp: c.sp, req: notice})
			if prod.handled != tc.handled {
				t.Fatalf("handled = %v (%+v, %v), want %v", prod.handled, prod, err, tc.handled)
			}
			if tc.refusal != "" {
				if !errors.Is(err, errCLILaneUnavailable) || !strings.Contains(err.Error(), tc.refusal) {
					t.Fatalf("err = %v, want the refusal naming %q", err, tc.refusal)
				}
				if ops := c.ops(t); len(ops) != 0 {
					t.Fatalf("op rows %+v after a refusal", ops)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if !tc.handled {
				if ops := c.ops(t); len(ops) != 0 || len(c.rings) != 0 {
					t.Fatalf("op rows %+v, rings %v on the legacy path", ops, c.rings)
				}
				return
			}
			op := c.noticeBy(t, slingNoticePrincipal, "gc-bead-1")
			if prod.op.ID != op.ID || !prod.created || op.Conversation != c.id {
				t.Fatalf("produced %+v, stored %+v; want the notice on the fixture's session", prod, op)
			}
		})
	}
}

// gc sling --nudge of a rig bead to a pool agent under lane authority reads
// the city's store for the pool's sessions, not the rig's, through the same
// routing (Rule 1): the running instance's notice lands in the city's
// sessions class, and nothing is queued.
func TestDoSlingNudgePoolUnderLaneRoutesThroughTheCityStore(t *testing.T) {
	c := newCLILaneCity(t, func(meta map[string]string) { meta["pool_slot"] = "1" })
	c.cfg.Workspace.Name = "test-city"
	writeSlingTestCity(t, c.dir, "[workspace]\nname = \"test-city\"\n")
	rig := &beads.MemStore{IDPrefix: "rg", HonorExplicitIDs: true}
	opened := 0
	prevOpen := slingOpenCityStore
	slingOpenCityStore = func(string) (beads.Store, error) {
		opened++
		return c.store, nil
	}
	t.Cleanup(func() { slingOpenCityStore = prevOpen })
	prevPoller := startNudgePoller
	startNudgePoller = func(_, _, _ string) error { return nil }
	t.Cleanup(func() { startNudgePoller = prevPoller })
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(2)}
	c.cfg.Agents = []config.Agent{a}
	var stdout, stderr bytes.Buffer
	calls := len(c.nudges())
	doSlingNudge(&a, "test-city", c.dir, c.cfg, c.sp, rig, "rg-work-1", &stdout, &stderr)
	if !strings.Contains(stdout.String(), "in the conversation lane") || stderr.Len() != 0 {
		t.Fatalf("stdout %q, stderr %q; want the notice proposed", stdout.String(), stderr.String())
	}
	if opened != 1 {
		t.Fatalf("city store opened %d times, want once", opened)
	}
	if ops := cliOpRows(t, rig); len(ops) != 0 {
		t.Fatalf("op rows in the rig store %+v, want none", ops)
	}
	if n := len(c.nudges()); n != calls || c.queuedItems(t) != 0 {
		t.Fatalf("legacy delivery ran: %d nudges, %d queued", n-calls, c.queuedItems(t))
	}
	c.noticeBy(t, slingNoticePrincipal, "rg-work-1")
}

// deliverSlingNudge routes its notice over the city's store it is handed,
// never over the bead's store its legacy path enqueues on (Rule 1): a target
// a rig's store resolved, which names no session id, finds its row in the
// city's store by name, and the notice lands there.
func TestDeliverSlingNudgeRoutesOverTheCityStoreNotTheBeads(t *testing.T) {
	c := newCLILaneCity(t)
	rig := &beads.MemStore{IDPrefix: "rg", HonorExplicitIDs: true}
	target := c.target
	target.sessionID = ""
	var stdout, stderr bytes.Buffer
	deliverSlingNudge(target, c.sp, rig, c.store, c.dir, "rg-work-1", &stdout, &stderr)
	if !strings.Contains(stdout.String(), "in the conversation lane") || stderr.Len() != 0 {
		t.Fatalf("stdout %q, stderr %q; want the notice proposed", stdout.String(), stderr.String())
	}
	if ops := cliOpRows(t, rig); len(ops) != 0 {
		t.Fatalf("op rows in the rig store %+v, want none", ops)
	}
	if n := c.queuedItems(t); n != 0 {
		t.Fatalf("%d queued, want none", n)
	}
	c.noticeBy(t, slingNoticePrincipal, "rg-work-1")
}
