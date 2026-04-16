package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleRigCreate(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := New(fs)

	body := `{"name":"new-rig","path":"/tmp/new-rig"}`
	req := newPostRequest("/v0/rigs", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

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
	srv := New(fs)

	body := `{"path":"/tmp/x"}`
	req := newPostRequest("/v0/rigs", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnprocessableEntity)
	}
}

func TestHandleRigCreate_MissingPath(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := New(fs)

	body := `{"name":"x"}`
	req := newPostRequest("/v0/rigs", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnprocessableEntity)
	}
}

func TestHandleRigUpdate(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := New(fs)

	body := `{"path":"/tmp/updated"}`
	req := httptest.NewRequest("PATCH", "/v0/rig/myrig", strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

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
	srv := New(fs)

	body := `{"path":"/tmp/x"}`
	req := httptest.NewRequest("PATCH", "/v0/rig/nonexistent", strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleRigDelete(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := New(fs)

	req := httptest.NewRequest("DELETE", "/v0/rig/myrig", nil)
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

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
	srv := New(fs)

	req := httptest.NewRequest("DELETE", "/v0/rig/nonexistent", nil)
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}
