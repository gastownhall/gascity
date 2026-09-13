package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleRigCreate(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	body := `{"name":"new-rig","path":"/tmp/new-rig"}`
	req := newPostRequest(cityURL(fs, "/rigs"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusCreated, w.Body.String())
	}

	found := false
	for _, r := range fs.cfg.Rigs {
		if r.Name == "new-rig" && r.Path == "/tmp/new-rig" {
			found = true
		}
	}
	if !found {
		t.Error("rig 'new-rig' not found in config after create")
	}
}

func TestHandleRigCreate_MissingName(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	body := `{"path":"/tmp/x"}`
	req := newPostRequest(cityURL(fs, "/rigs"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnprocessableEntity)
	}
}

func TestHandleRigCreate_MissingPath(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	body := `{"name":"x"}`
	req := newPostRequest(cityURL(fs, "/rigs"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnprocessableEntity)
	}
}

func TestHandleRigUpdate(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	body := `{"path":"/tmp/updated"}`
	req := httptest.NewRequest("PATCH", cityURL(fs, "/rig/myrig"), strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	for _, r := range fs.cfg.Rigs {
		if r.Name == "myrig" {
			if r.Path != "/tmp/updated" {
				t.Errorf("path = %q, want %q", r.Path, "/tmp/updated")
			}
			return
		}
	}
	t.Error("rig 'myrig' not found after update")
}

func TestHandleRigUpdate_NotFound(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	body := `{"path":"/tmp/x"}`
	req := httptest.NewRequest("PATCH", cityURL(fs, "/rig/nonexistent"), strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleRigDelete(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("DELETE", cityURL(fs, "/rig/myrig"), nil)
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	for _, r := range fs.cfg.Rigs {
		if r.Name == "myrig" {
			t.Error("rig 'myrig' still exists after delete")
		}
	}
}

func TestHandleRigDelete_NotFound(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("DELETE", cityURL(fs, "/rig/nonexistent"), nil)
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}
