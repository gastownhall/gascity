package api

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/configedit"
)

// TestMutationError pins the status-preserving mapping from the configedit
// sentinels to typed apierr problem types for the shared create/update/delete
// helper. Each status must match what the case returned before the apierr port
// (404/409/400/500) while the body now carries a stable machine code, and the
// not-found case must use the caller's resource-specific 404 so a client can
// branch on which resource was missing. Detail stays byte-identical to the
// domain error.
func TestMutationError(t *testing.T) {
	notFound := apierr.RigNotFound
	cases := []struct {
		name       string
		err        error
		wantCode   string
		wantStatus int
	}{
		{"not-found", fmt.Errorf("wrap: %w", configedit.ErrNotFound), "rig-not-found", http.StatusNotFound},
		{"already-exists", fmt.Errorf("wrap: %w", configedit.ErrAlreadyExists), "conflict-wrong-state", http.StatusConflict},
		{"pack-derived", fmt.Errorf("wrap: %w", configedit.ErrPackDerived), "conflict-wrong-state", http.StatusConflict},
		{"validation", fmt.Errorf("wrap: %w", configedit.ErrValidation), "invalid-request", http.StatusBadRequest},
		{"internal", errors.New("boom"), "internal", http.StatusInternalServerError},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := mutationError(tc.err, notFound)
			var em *apierr.ErrorModel
			if !errors.As(got, &em) {
				t.Fatalf("mutationError returned %T, want *apierr.ErrorModel", got)
			}
			if em.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", em.Code, tc.wantCode)
			}
			if em.Status != tc.wantStatus {
				t.Errorf("status = %d, want %d", em.Status, tc.wantStatus)
			}
			if want := "urn:gascity:error:" + tc.wantCode; em.Type != want {
				t.Errorf("type = %q, want %q", em.Type, want)
			}
			if em.Detail != tc.err.Error() {
				t.Errorf("detail = %q, want %q", em.Detail, tc.err.Error())
			}
		})
	}
}

// TestErrMutationsNotSupported pins the shared not-supported sentinel to the
// typed not-implemented problem type at 501, so a state without StateMutator
// still returns a machine-identifiable body.
func TestErrMutationsNotSupported(t *testing.T) {
	var em *apierr.ErrorModel
	if !errors.As(error(errMutationsNotSupported), &em) {
		t.Fatalf("errMutationsNotSupported is %T, want *apierr.ErrorModel", errMutationsNotSupported)
	}
	if em.Code != "not-implemented" {
		t.Errorf("code = %q, want not-implemented", em.Code)
	}
	if em.Status != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d", em.Status, http.StatusNotImplemented)
	}
}
