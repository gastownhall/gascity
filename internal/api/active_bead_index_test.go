package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestActiveBeadIndexPrecedenceAndScope pins the index's lookup order and
// scope: identity priority first, then store order, then newest-first within
// a store; exact assignee matching; a scoped lookup reads only its rig's
// store; a lookup with no identities loads nothing. Cold-rig exclusion is
// pinned by the suspendedRigLookup suite, which runs through the same index.
func TestActiveBeadIndexPrecedenceAndScope(t *testing.T) {
	newScopeState := func(t *testing.T) (*fakeState, *activeWorkCountingStore, *activeWorkCountingStore, *activeWorkCountingStore) {
		t.Helper()
		fs := newFakeState(t)
		early := &activeWorkCountingStore{Store: beads.NewMemStore()} // "aaa-rig"
		late := &activeWorkCountingStore{Store: beads.NewMemStore()}  // "zzz-rig"
		city := &activeWorkCountingStore{Store: beads.NewMemStore()}  // "test-city"
		fs.stores = map[string]beads.Store{"aaa-rig": early, "zzz-rig": late}
		fs.cityBeadStore = city
		return fs, early, late, city
	}

	t.Run("identity first, then store order, then newest within store", func(t *testing.T) {
		fs, early, late, city := newScopeState(t)
		mustCreateInProgress(t, early.Store, "older rig task", "worker")
		newer := mustCreateInProgress(t, early.Store, "newer rig task", "worker")
		mustCreateInProgress(t, late.Store, "late task", "worker")
		cityFallback := mustCreateInProgress(t, city.Store, "city fallback", "fallback")
		lateFallback := mustCreateInProgress(t, late.Store, "late fallback", "fallback")
		citySession := mustCreateInProgress(t, city.Store, "city work for session id", "session-id")
		rigTemplate := mustCreateInProgress(t, early.Store, "rig work for template", "template")

		idx := New(fs).newActiveBeadIndex()
		// Store order: aaa-rig sorts before test-city and zzz-rig, so the
		// newest aaa-rig bead wins over the later stores'.
		if got := idx.lookup("", "worker"); got != newer.ID {
			t.Errorf("lookup(worker) = %q, want aaa-rig's newest %s", got, newer.ID)
		}
		// Store order continues past the first store: test-city sorts before
		// zzz-rig, so the city's fallback bead outranks the rig's.
		if got := idx.lookup("", "fallback"); got != cityFallback.ID {
			t.Errorf("lookup(fallback) = %q, want test-city's %s over zzz-rig's %s", got, cityFallback.ID, lateFallback.ID)
		}
		// Identity order: the first identity's match wins even when a later
		// identity's bead sits in a store that sorts first.
		if got := idx.lookup("", "session-id", "template"); got != citySession.ID {
			t.Errorf("lookup(session-id, template) = %q, want session-id's %s over template's %s", got, citySession.ID, rigTemplate.ID)
		}
		if got := idx.lookup("", "template", "session-id"); got != rigTemplate.ID {
			t.Errorf("lookup(template, session-id) = %q, want template's %s", got, rigTemplate.ID)
		}
	})

	t.Run("exact assignee match", func(t *testing.T) {
		fs, early, _, _ := newScopeState(t)
		prefixed := mustCreateInProgress(t, early.Store, "other session's work", "worker-2")

		idx := New(fs).newActiveBeadIndex()
		if got := idx.lookup("", "worker"); got != "" {
			t.Errorf("lookup(worker) = %q, want none: matching is exact, and the store holds %s for worker-2", got, prefixed.ID)
		}
		if got := idx.lookup("", "worker-2"); got != prefixed.ID {
			t.Errorf("lookup(worker-2) = %q, want %s", got, prefixed.ID)
		}
	})

	t.Run("scoped lookup reads only its rig's store", func(t *testing.T) {
		fs, early, late, city := newScopeState(t)
		earlyWork := mustCreateInProgress(t, early.Store, "aaa work", "worker")
		zzzWork := mustCreateInProgress(t, late.Store, "zzz work", "worker")
		mustCreateInProgress(t, city.Store, "city work", "worker")

		idx := New(fs).newActiveBeadIndex()
		if got := idx.lookup("zzz-rig", "worker"); got != zzzWork.ID {
			t.Errorf("scoped lookup = %q, want zzz-rig's %s, not aaa-rig's %s", got, zzzWork.ID, earlyWork.ID)
		}
		if early.activeReads != 0 || city.activeReads != 0 {
			t.Errorf("a lookup scoped to zzz-rig read other stores: aaa-rig %d, test-city %d", early.activeReads, city.activeReads)
		}
	})

	t.Run("no identities loads nothing", func(t *testing.T) {
		fs, early, late, city := newScopeState(t)
		mustCreateInProgress(t, early.Store, "aaa work", "worker")

		idx := New(fs).newActiveBeadIndex()
		if got := idx.lookup("", ""); got != "" {
			t.Errorf("lookup of no identities = %q, want empty", got)
		}
		if early.activeReads != 0 || late.activeReads != 0 || city.activeReads != 0 {
			t.Errorf("a lookup with no identities read stores: aaa-rig %d, zzz-rig %d, test-city %d", early.activeReads, late.activeReads, city.activeReads)
		}
	})
}

// activeWorkFailingStore fails the store's in_progress reads with fail,
// keeping rows when it reports a partial result.
type activeWorkFailingStore struct {
	beads.Store
	fail  error
	rows  []beads.Bead
	calls int
}

func (s *activeWorkFailingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Status == "in_progress" {
		s.calls++
		if s.fail != nil {
			return s.rows, s.fail
		}
	}
	return s.Store.List(query)
}

// sessionRowWire is a session list row's id and active bead.
type sessionRowWire struct {
	ID         string `json:"id"`
	ActiveBead string `json:"active_bead"`
}

// sessionListWire is a session list body's partial shape.
type sessionListWire struct {
	Items         []sessionRowWire `json:"items"`
	Total         int              `json:"total"`
	Partial       bool             `json:"partial"`
	PartialErrors []string         `json:"partial_errors"`
}

// TestActiveBeadIndexPartialErrors surfaces a failed store read as the
// response's partial errors instead of swallowing it: the list answers
// partial, names the rig, is not cached (the next request re-reads the
// store), and a partial result's decoded rows still answer ActiveBead.
func TestActiveBeadIndexPartialErrors(t *testing.T) {
	fail := errors.New("bd list: store unavailable")
	wantErr := func(t *testing.T, errs []string) {
		t.Helper()
		joined := strings.Join(errs, "; ")
		if !strings.Contains(joined, "myrig") || !strings.Contains(joined, "store unavailable") {
			t.Errorf("partial errors %q, want one naming rig myrig and the failure", joined)
		}
	}

	t.Run("agent list surfaces the failure and re-reads next request", func(t *testing.T) {
		st := newFakeState(t)
		failing := &activeWorkFailingStore{Store: st.stores["myrig"], fail: fail}
		st.stores["myrig"] = failing

		first := getAgentList(t, st, "")
		if !first.Partial {
			t.Errorf("GET /agents with a failing bead store answered partial=false, want true (%v)", first.PartialErrors)
		}
		wantErr(t, first.PartialErrors)

		second := getAgentList(t, st, "")
		if failing.calls != 2 {
			t.Errorf("the second GET /agents made %d in_progress reads, want 2: a partial list must not be served from cache", failing.calls)
		}
		if !second.Partial {
			t.Errorf("the re-read answered partial=false, want true (%v)", second.PartialErrors)
		}
		wantErr(t, second.PartialErrors)
	})

	for _, tc := range []struct {
		name   string
		legacy bool
	}{{"huma session list", false}, {"legacy session list", true}} {
		t.Run(tc.name+" surfaces the failure and re-reads next request", func(t *testing.T) {
			fs := newSessionFakeState(t)
			createTestSession(t, fs.cityBeadStore, fs.sp, "Session")
			failing := &activeWorkFailingStore{Store: fs.stores["myrig"], fail: fail}
			fs.stores["myrig"] = failing
			srv := New(fs)
			h := newTestCityHandlerWith(t, fs, srv)

			fetch := func() sessionListWire {
				rec := httptest.NewRecorder()
				if tc.legacy {
					srv.legacySessionHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/sessions", nil))
				} else {
					h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(fs, "/sessions"), nil))
				}
				if rec.Code != http.StatusOK {
					t.Fatalf("GET status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
				}
				var body sessionListWire
				if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
					t.Fatalf("decode: %v", err)
				}
				return body
			}

			first := fetch()
			if !first.Partial {
				t.Errorf("%s with a failing bead store answered partial=false, want true (%v)", tc.name, first.PartialErrors)
			}
			wantErr(t, first.PartialErrors)

			fetch()
			if failing.calls != 2 {
				t.Errorf("the second read made %d in_progress reads, want 2", failing.calls)
			}
		})

		t.Run(tc.name+" keeps a partial result's decoded rows", func(t *testing.T) {
			fs := newSessionFakeState(t)
			info := createTestSession(t, fs.cityBeadStore, fs.sp, "Session")
			rows := []beads.Bead{{ID: "w-partial-1", Assignee: info.ID, Status: "in_progress"}}
			failing := &activeWorkFailingStore{
				Store: fs.stores["myrig"],
				fail:  &beads.PartialResultError{Op: "bd list", Err: fail},
				rows:  rows,
			}
			fs.stores["myrig"] = failing
			srv := New(fs)
			h := newTestCityHandlerWith(t, fs, srv)

			rec := httptest.NewRecorder()
			if tc.legacy {
				srv.legacySessionHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/sessions", nil))
			} else {
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(fs, "/sessions"), nil))
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("GET status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
			}
			var body sessionListWire
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !body.Partial {
				t.Errorf("%s answered partial=false, want true: a partial read is an error (%v)", tc.name, body.PartialErrors)
			}
			wantErr(t, body.PartialErrors)
			var found bool
			for _, item := range body.Items {
				if item.ID != info.ID {
					continue
				}
				found = true
				if item.ActiveBead != "w-partial-1" {
					t.Errorf("%s active_bead = %q, want the decoded partial row w-partial-1", tc.name, item.ActiveBead)
				}
			}
			if !found {
				t.Fatalf("session %s not listed", info.ID)
			}
		})
	}
}
