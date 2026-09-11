package apierr

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

// ErrorModel must satisfy huma.StatusError so it can be returned from handlers
// and thrown by the huma.NewError override.
var _ huma.StatusError = (*ErrorModel)(nil)

func TestRegister_RejectsMalformedCode(t *testing.T) {
	for _, bad := range []string{"", "Bad", "has_underscore", "-leading", "trailing-", "double--dash", "UPPER"} {
		t.Run(bad, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("Register(%q) should panic on malformed code", bad)
				}
			}()
			Register(ProblemType{Code: bad, Status: 400, Title: "x"})
		})
	}
}

func TestRegister_RejectsBadStatusOrTitle(t *testing.T) {
	cases := []ProblemType{
		{Code: "code-a", Status: 200, Title: "ok"}, // non-4xx/5xx
		{Code: "code-b", Status: 400, Title: ""},   // empty title
	}
	for _, pt := range cases {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("Register(%+v) should panic", pt)
				}
			}()
			Register(pt)
		}()
	}
}

func TestRegister_IdempotentVsConflict(t *testing.T) {
	pt := ProblemType{Code: "dup-test-code", Status: 409, Title: "Dup"}
	Register(pt)
	Register(pt) // identical re-register: no-op, must not panic

	defer func() {
		if recover() == nil {
			t.Fatal("conflicting re-register must panic")
		}
	}()
	Register(ProblemType{Code: "dup-test-code", Status: 400, Title: "Different"})
}

func TestLookupAndURN(t *testing.T) {
	pt, ok := Lookup("bead-not-found")
	if !ok || pt.Status != http.StatusNotFound {
		t.Fatalf("Lookup(bead-not-found) = %+v,%v", pt, ok)
	}
	if pt.URN() != "urn:gascity:error:bead-not-found" {
		t.Fatalf("URN = %q", pt.URN())
	}
	got, ok := LookupURN("urn:gascity:error:bead-not-found")
	if !ok || got != pt {
		t.Fatalf("LookupURN = %+v,%v", got, ok)
	}
	if _, ok := LookupURN("urn:gascity:error:nope"); ok {
		t.Fatal("LookupURN of unregistered code should be false")
	}
	if _, ok := LookupURN("bead-not-found"); ok {
		t.Fatal("LookupURN of a bare code (no prefix) should be false")
	}
}

// The three original sling URNs must stay byte-identical — they are already
// public in the OpenAPI spec via x-gascity-problem-types.
func TestFrozenSlingURNs(t *testing.T) {
	for _, want := range []string{
		"urn:gascity:error:sling-missing-bead",
		"urn:gascity:error:sling-cross-rig",
		"urn:gascity:error:sling-cross-store-route",
	} {
		if _, ok := LookupURN(want); !ok {
			t.Fatalf("frozen URN %q missing from the registry", want)
		}
	}
}

func TestRegisteredIsSorted(t *testing.T) {
	reg := Registered()
	for i := 1; i < len(reg); i++ {
		if reg[i-1].Code >= reg[i].Code {
			t.Fatalf("Registered() not sorted by code at %d: %q >= %q", i, reg[i-1].Code, reg[i].Code)
		}
	}
}

func TestConstructorsStampTypeCodeStatusTitle(t *testing.T) {
	e := BeadNotFound.Msg("bead bd-1 not found")
	if e.Type != "urn:gascity:error:bead-not-found" || e.Code != "bead-not-found" {
		t.Fatalf("Msg type/code = %q/%q", e.Type, e.Code)
	}
	if e.Status != http.StatusNotFound || e.Title != "Bead Not Found" || e.Detail != "bead bd-1 not found" {
		t.Fatalf("Msg status/title/detail = %d/%q/%q", e.Status, e.Title, e.Detail)
	}
	if e.GetStatus() != http.StatusNotFound {
		t.Fatalf("GetStatus = %d (StatusError not satisfied via embedding)", e.GetStatus())
	}

	if got := InvalidRequest.Msgf("field %q required", "name").Detail; got != `field "name" required` {
		t.Fatalf("Msgf detail = %q", got)
	}

	withList := ConflictWrongState.With("conflict", &huma.ErrorDetail{Message: "d1"})
	if len(withList.Errors) != 1 || withList.Errors[0].Message != "d1" {
		t.Fatalf("With errors = %+v", withList.Errors)
	}

	ws := StoreUnavailable.WithStatus(http.StatusInternalServerError, "boom")
	if ws.Status != http.StatusInternalServerError || ws.Code != "store-unavailable" {
		t.Fatalf("WithStatus status/code = %d/%q", ws.Status, ws.Code)
	}
}

// The wire shape must flatten the embedded huma.ErrorModel and add `code`.
func TestErrorModelJSONShape(t *testing.T) {
	b, err := json.Marshal(BeadNotFound.Msg("nope"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for k, want := range map[string]any{
		"type":   "urn:gascity:error:bead-not-found",
		"code":   "bead-not-found",
		"title":  "Bead Not Found",
		"detail": "nope",
		"status": float64(http.StatusNotFound),
	} {
		if m[k] != want {
			t.Fatalf("json[%q] = %v (%T), want %v", k, m[k], m[k], want)
		}
	}
	// code is omitempty: an empty-code model omits it (defends the wire compat
	// claim for legacy paths that don't stamp a code).
	b2, _ := json.Marshal(&ErrorModel{})
	if json.Valid(b2) && contains(string(b2), `"code"`) {
		t.Fatalf("empty ErrorModel must omit code, got %s", b2)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
