package api

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// failingBeadStore wraps an in-memory bead store and forces List/Ready to
// fail. Used to verify list handlers surface store errors as
// Partial/PartialErrors instead of silently dropping the rig.
type failingBeadStore struct {
	beads.Store
	listErr  error
	readyErr error
}

func (f *failingBeadStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.Store.List(q)
}

func (f *failingBeadStore) Ready() ([]beads.Bead, error) {
	if f.readyErr != nil {
		return nil, f.readyErr
	}
	return f.Store.Ready()
}

func newPartialListState(t *testing.T, listErr, readyErr error) *fakeState {
	t.Helper()
	fs := newFakeState(t)

	// Add a second rig "bad" whose store fails.
	bad := beads.NewMemStore()
	_, _ = bad.Create(beads.Bead{Type: "task", Title: "would-be-lost", Status: "active"})
	wrapped := &failingBeadStore{Store: bad, listErr: listErr, readyErr: readyErr}
	fs.stores["bad"] = wrapped
	fs.cfg.Rigs = append(fs.cfg.Rigs, config.Rig{Name: "bad", Path: t.TempDir()})

	// Seed "myrig" with a real bead so the good-rig path has something.
	_, _ = fs.stores["myrig"].Create(beads.Bead{Type: "task", Title: "ok-rig-task", Status: "active"})
	return fs
}

func TestBeadListSurfacesStoreErrorsAsPartial(t *testing.T) {
	boom := errors.New("disk is on fire")
	fs := newPartialListState(t, boom, nil)
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/beads"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (handler should degrade, not fail)", rec.Code)
	}

	var body struct {
		Items         []beads.Bead `json:"items"`
		Partial       bool         `json:"partial"`
		PartialErrors []string     `json:"partial_errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
	}
	if !body.Partial {
		t.Errorf("Partial = false, want true (bad rig should have failed)")
	}
	if len(body.PartialErrors) == 0 {
		t.Errorf("PartialErrors empty, want at least one entry")
	}
	if len(body.Items) == 0 {
		t.Errorf("Items empty, want the good rig's bead to survive")
	}
}

// When EVERY rig store fails, returning 200 + empty + partial=true
// conflates outage with "no data". The handler must return 503 so
// clients can tell the difference.
func TestBeadListReturns503OnTotalOutage(t *testing.T) {
	fs := newFakeState(t)
	boom := errors.New("disk is on fire")
	// Wrap myrig (the only rig) so its store always errors.
	wrapped := &failingBeadStore{Store: fs.stores["myrig"], listErr: boom}
	fs.stores["myrig"] = wrapped

	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest("GET", cityURL(fs, "/beads"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("status = %d, want 503 when every backend fails (body=%q)", rec.Code, rec.Body.String())
	}
}

func TestBeadReadySurfacesStoreErrorsAsPartial(t *testing.T) {
	boom := errors.New("ready: disk is on fire")
	fs := newPartialListState(t, nil, boom)
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/beads/ready"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body struct {
		Partial       bool     `json:"partial"`
		PartialErrors []string `json:"partial_errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
	}
	if !body.Partial {
		t.Errorf("Partial = false, want true")
	}
	if len(body.PartialErrors) == 0 {
		t.Errorf("PartialErrors empty")
	}
}
