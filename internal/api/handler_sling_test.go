package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestSlingWithBead(t *testing.T) {
	state := newFakeMutatorState(t)
	srv := New(state)

	oldRunner := slingCommandRunner
	defer func() { slingCommandRunner = oldRunner }()

	var gotArgs []string
	slingCommandRunner = func(_ context.Context, _ string, args []string) (string, string, error) {
		gotArgs = args
		return "Slung test-1 → myrig/worker\n", "", nil
	}

	body := `{"target":"myrig/worker","bead":"test-1"}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/sling", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "slung" {
		t.Fatalf("status = %q, want %q", resp["status"], "slung")
	}
	if resp["mode"] != "direct" {
		t.Fatalf("mode = %q, want %q", resp["mode"], "direct")
	}
	// Verify CLI args: --city <path> sling <target> <bead>
	if len(gotArgs) < 4 || gotArgs[2] != "sling" || gotArgs[3] != "myrig/worker" || gotArgs[4] != "test-1" {
		t.Fatalf("unexpected args: %v", gotArgs)
	}
}

func TestSlingMissingTarget(t *testing.T) {
	state := newFakeMutatorState(t)
	srv := New(state)

	body := `{"bead":"abc"}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/sling", strings.NewReader(body)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestSlingTargetNotFound(t *testing.T) {
	state := newFakeMutatorState(t)
	srv := New(state)

	body := `{"target":"nonexistent","bead":"abc"}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/sling", strings.NewReader(body)))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestSlingMissingBeadAndFormula(t *testing.T) {
	state := newFakeMutatorState(t)
	srv := New(state)

	body := `{"target":"myrig/worker"}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/sling", strings.NewReader(body)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestSlingBeadAndFormulaMutuallyExclusive(t *testing.T) {
	state := newFakeMutatorState(t)
	srv := New(state)

	body := `{"target":"myrig/worker","bead":"abc","formula":"xyz"}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/sling", strings.NewReader(body)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestSlingBeadNotFound(t *testing.T) {
	state := newFakeMutatorState(t)
	srv := New(state)

	oldRunner := slingCommandRunner
	defer func() { slingCommandRunner = oldRunner }()

	slingCommandRunner = func(_ context.Context, _ string, _ []string) (string, string, error) {
		return "", "bead nonexistent not found", errors.New("exit status 1")
	}

	body := `{"target":"myrig/worker","bead":"nonexistent"}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/sling", strings.NewReader(body)))

	// gc sling returns non-zero for missing beads; HTTP handler surfaces as 400.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestSlingFormulaDelegatesToGcSling(t *testing.T) {
	state := newFakeMutatorState(t)
	srv := New(state)

	oldRunner := slingCommandRunner
	defer func() { slingCommandRunner = oldRunner }()

	var gotCityPath string
	var gotArgs []string
	slingCommandRunner = func(_ context.Context, cityPath string, args []string) (string, string, error) {
		gotCityPath = cityPath
		gotArgs = append([]string(nil), args...)
		return "Started workflow wf_123 (formula \"mol-review\") → myrig/worker\n", "", nil
	}

	body := `{"target":"myrig/worker","formula":"mol-review","vars":{"pr_url":"https://example.test/pr/123"}}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/sling", strings.NewReader(body)))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	if gotCityPath != state.CityPath() {
		t.Fatalf("cityPath = %q, want %q", gotCityPath, state.CityPath())
	}
	wantArgs := []string{
		"--city", state.CityPath(),
		"sling", "myrig/worker", "mol-review", "--formula",
		"--var", "pr_url=https://example.test/pr/123",
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("args = %#v, want %#v", gotArgs, wantArgs)
	}

	var resp slingResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.WorkflowID != "wf_123" || resp.RootBeadID != "wf_123" {
		t.Fatalf("response = %+v, want workflow/root wf_123", resp)
	}
	if resp.Mode != "standalone" {
		t.Fatalf("mode = %q, want %q", resp.Mode, "standalone")
	}
}

func TestSlingAttachedFormulaDelegatesToGcSling(t *testing.T) {
	state := newFakeMutatorState(t)
	srv := New(state)

	oldRunner := slingCommandRunner
	defer func() { slingCommandRunner = oldRunner }()

	var gotArgs []string
	slingCommandRunner = func(_ context.Context, _ string, args []string) (string, string, error) {
		gotArgs = append([]string(nil), args...)
		return "Attached workflow wf_456 (formula \"mol-review\") to BD-42\n", "", nil
	}

	body := `{"target":"myrig/worker","formula":"mol-review","attached_bead_id":"BD-42","vars":{"issue":"BD-42"}}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/sling", strings.NewReader(body)))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	wantArgs := []string{
		"--city", state.CityPath(),
		"sling", "myrig/worker", "BD-42", "--on", "mol-review",
		"--var", "issue=BD-42",
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("args = %#v, want %#v", gotArgs, wantArgs)
	}

	var resp slingResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Mode != "attached" || resp.AttachedBeadID != "BD-42" {
		t.Fatalf("response = %+v, want attached workflow on BD-42", resp)
	}
}

func TestSlingRejectsVarsWithoutFormula(t *testing.T) {
	state := newFakeMutatorState(t)
	srv := New(state)

	body := `{"target":"myrig/worker","bead":"BD-42","vars":{"issue":"BD-42"}}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/sling", strings.NewReader(body)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestSlingFormulaRunnerErrorSurfacesAsBadRequest(t *testing.T) {
	state := newFakeMutatorState(t)
	srv := New(state)

	oldRunner := slingCommandRunner
	defer func() { slingCommandRunner = oldRunner }()

	slingCommandRunner = func(_ context.Context, _ string, _ []string) (string, string, error) {
		return "", "gc sling: could not resolve session name", errors.New("exit status 1")
	}

	body := `{"target":"myrig/worker","formula":"mol-review"}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/sling", strings.NewReader(body)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "could not resolve session name") {
		t.Fatalf("body = %s, want session resolution error", rec.Body.String())
	}
}
