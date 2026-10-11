package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/session"
)

// TestMailQueriesForASessionReadItsSessionInbox pins that the API answers a
// session's mail list and counts from the same mailbox the CLI's self-reads,
// hook injection and SessionStart mail use (beadmail.Provider.InboxForSession):
// the session's own routes, a closed predecessor's routes (here the holder of
// the earlier alias "old-sky"), and never a live session's (here the holder of
// "older-sky") — not a per-address expansion that unions every holder's routes.
func TestMailQueriesForASessionReadItsSessionInbox(t *testing.T) {
	state := newFakeState(t)
	store := beads.NewMemStore()
	state.cityBeadStore = store
	state.stores = map[string]beads.Store{"myrig": store}
	mp := beadmail.New(store)
	state.cityMailProv = mp
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
	read, err := mp.Send("human", "sky", "", "already read")
	if err != nil {
		t.Fatalf("Send(read): %v", err)
	}
	if err := mp.MarkRead(read.ID); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	wantUnread := []string{sent["sky"], sent[predecessor], sent["runtime-pred"]}
	sort.Strings(wantUnread)
	wantAll := append(slices.Clone(wantUnread), read.ID)
	sort.Strings(wantAll)

	h := newTestCityHandler(t, state)
	list := func(query string) []string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/mail?"+query), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /mail?%s = %d: %s", query, rec.Code, rec.Body.String())
		}
		var body MailListBody
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("decoding /mail?%s: %v", query, err)
		}
		ids := make([]string, 0, len(body.Items))
		for _, m := range body.Items {
			ids = append(ids, m.ID)
		}
		sort.Strings(ids)
		return ids
	}
	if got := list("agent=" + self); !slices.Equal(got, wantUnread) {
		t.Errorf("unread mail list = %v, want the session inbox %v", got, wantUnread)
	}
	if got := list("agent=" + self + "&status=all"); !slices.Equal(got, wantAll) {
		t.Errorf("all mail list = %v, want the session inbox %v", got, wantAll)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/mail/count?agent="+self), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /mail/count = %d: %s", rec.Code, rec.Body.String())
	}
	var counts struct {
		Total  int `json:"total"`
		Unread int `json:"unread"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&counts); err != nil {
		t.Fatalf("decoding /mail/count: %v", err)
	}
	if counts.Total != len(wantAll) || counts.Unread != len(wantUnread) {
		t.Errorf("mail count = %d total, %d unread; want %d, %d", counts.Total, counts.Unread, len(wantAll), len(wantUnread))
	}
}
