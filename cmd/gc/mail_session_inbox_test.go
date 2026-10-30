package main

import (
	"bytes"
	"encoding/json"
	"io"
	"slices"
	"sort"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/session"
)

// TestSelfReadsAgreeOnTheSessionInbox pins that every read of a session's own
// mailbox — mail check, its injection, SessionStart's ordinary-mail block,
// mail inbox and mail count — sees the same mail, whether the session was
// resolved by its id or by its alias, defined by beadmail.Provider.InboxForSession:
// the session's routes, a closed predecessor's routes (here the holder of the
// earlier alias "old-sky"), and never a live session's (here the holder of
// "older-sky").
//
// With message listed in [ops] authoritative the city's mail provider carries
// the lane notice withdrawal (withMailNoticeWithdrawal), and every self-read
// still answers from the session inbox.
func TestSelfReadsAgreeOnTheSessionInbox(t *testing.T) {
	for name, authoritative := range map[string][]string{"message unlisted": nil, "message listed": {"message"}} {
		t.Run(name, func(t *testing.T) {
			cfg := &config.City{}
			cfg.Ops.Authoritative = authoritative
			selfReadsAgreeOnTheSessionInbox(t, func(p mail.Provider, store beads.Store) mail.Provider {
				return withMailNoticeWithdrawal(p, store, cfg, io.Discard)
			})
		})
	}
}

func selfReadsAgreeOnTheSessionInbox(t *testing.T, wrap func(mail.Provider, beads.Store) mail.Provider) {
	clearGCEnv(t)
	store := beads.NewMemStore()
	mp := wrap(beadmail.New(store), store)
	createSession := func(metadata map[string]string, closed bool) string {
		t.Helper()
		b, err := store.Create(beads.Bead{Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: metadata})
		if err != nil {
			t.Fatalf("Create session: %v", err)
		}
		if closed {
			if err := store.Close(b.ID); err != nil {
				t.Fatalf("Close session: %v", err)
			}
		}
		return b.ID
	}
	self := createSession(map[string]string{"alias": "sky", "alias_history": "old-sky,older-sky", "session_name": "runtime-sky"}, false)
	predecessor := createSession(map[string]string{"alias": "old-sky", "session_name": "runtime-pred"}, true)
	live := createSession(map[string]string{"alias": "older-sky", "session_name": "runtime-live"}, false)
	sent := map[string]string{}
	for _, to := range []string{"sky", predecessor, "runtime-pred", live, "runtime-live"} {
		m, err := mp.Send("human", to, "", "for "+to)
		if err != nil {
			t.Fatalf("Send(%q): %v", to, err)
		}
		sent[to] = m.ID
	}
	want := []string{sent["sky"], sent[predecessor], sent["runtime-pred"]}
	sort.Strings(want)

	info, _, err := session.ResolveSessionRecordByExactID(store, self)
	if err != nil {
		t.Fatalf("ResolveSessionRecordByExactID: %v", err)
	}
	target, err := mailTargetFromSessionInfo(info, self)
	if err != nil {
		t.Fatalf("mailTargetFromSessionInfo: %v", err)
	}
	sortedIDs := func(messages []mail.Message) []string {
		ids := make([]string, 0, len(messages))
		for _, m := range messages {
			ids = append(ids, m.ID)
		}
		sort.Strings(ids)
		return ids
	}

	checked, inbox, err := unreadMailForTarget(mp, target)
	if err != nil {
		t.Fatalf("unreadMailForTarget: %v", err)
	}
	if got := sortedIDs(checked); !slices.Equal(got, want) {
		t.Fatalf("mail check sees %v, want %v", got, want)
	}

	var inboxOut, inboxErr bytes.Buffer
	if code := doMailInboxTargetWithJSON(mp, target, true, &inboxOut, &inboxErr); code != 0 {
		t.Fatalf("mail inbox = %d; stderr=%s", code, inboxErr.String())
	}
	var listed mailInboxJSONResult
	if err := json.Unmarshal(inboxOut.Bytes(), &listed); err != nil {
		t.Fatalf("decoding mail inbox JSON: %v\n%s", err, inboxOut.String())
	}
	if got := sortedIDs(listed.Messages); !slices.Equal(got, want) {
		t.Fatalf("mail inbox lists %v, want mail check's %v", got, want)
	}

	var countOut, countErr bytes.Buffer
	if code := doMailCountTargetWithJSON(mp, target, true, &countOut, &countErr); code != 0 {
		t.Fatalf("mail count = %d; stderr=%s", code, countErr.String())
	}
	var counted mailCountJSONResult
	if err := json.Unmarshal(countOut.Bytes(), &counted); err != nil {
		t.Fatalf("decoding mail count JSON: %v\n%s", err, countOut.String())
	}
	if counted.Total != len(want) || counted.Unread != len(want) {
		t.Fatalf("mail count = %d total, %d unread; want mail check's %d, %d", counted.Total, counted.Unread, len(want), len(want))
	}

	for name, resolve := range map[string]func() (resolvedMailTarget, error){
		"with config": func() (resolvedMailTarget, error) {
			return resolveMailTargetsWithConfig(t.TempDir(), &config.City{}, store, "sky")
		},
		"without config": func() (resolvedMailTarget, error) { return resolveMailTargets(store, "sky") },
	} {
		byAlias, err := resolve()
		if err != nil {
			t.Fatalf("resolving %q %s: %v", "sky", name, err)
		}
		got, _, err := unreadMailForTarget(mp, byAlias)
		if err != nil {
			t.Fatalf("unreadMailForTarget(alias %s): %v", name, err)
		}
		if ids := sortedIDs(got); !slices.Equal(ids, want) {
			t.Fatalf("mail check by alias %s sees %v, want the session inbox %v", name, ids, want)
		}
	}

	t.Setenv("GC_SESSION_ID", self)
	t.Setenv("GC_ALIAS", "sky")
	if got := sortedIDs(primeUnreadMailMessagesFromInbox(mp, inbox)); !slices.Equal(got, want) {
		t.Fatalf("SessionStart mail sees %v, want mail check's %v", got, want)
	}

	var injected bytes.Buffer
	if code := doMailCheckTargetWithFormat(mp, target, true, "", &injected, io.Discard); code != 0 {
		t.Fatalf("mail check --inject = %d", code)
	}
	for to, id := range sent {
		if slices.Contains(want, id) != bytes.Contains(injected.Bytes(), []byte(id)) {
			t.Errorf("injection disagrees with mail check on the mail for %q:\n%s", to, injected.String())
		}
	}
}
