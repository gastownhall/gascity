package beads

import (
	"errors"
	"strings"
	"testing"
)

// TestBdStoreGetTreatsHTTPNotFoundEnvelopeAsNotFound: against a remote bd
// serve (BEADS_SERVER_URL), `bd show <missing-id> --json` exits 1 with the
// structured error on stdout and only an unrelated "not served over HTTP" line
// on stderr. Classifying from stderr alone turned a plain absent bead into a
// hard error, which broke session address resolution (mail, handoff, prime)
// for remote workers.
func TestBdStoreGetTreatsHTTPNotFoundEnvelopeAsNotFound(t *testing.T) {
	runner := func(_, _ string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "show" {
			return []byte(`{"error": "no issues found matching the provided IDs", "schema_version": 1}`),
				errors.New("exit status 1: bd show is not supported against HTTP workspace http://100.64.0.1:7491: part of this command is not served over HTTP; run it in a local workspace")
		}
		return []byte("[]"), nil
	}
	s := NewBdStore("/city", runner)
	_, err := s.Get("mayor")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) err = %v, want ErrNotFound", err)
	}
}

// A genuine failure with no not-found envelope must stay an error.
func TestBdStoreGetKeepsRealFailures(t *testing.T) {
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return []byte(`{"error": "listReadyWork: bd serve answered 401 unauthenticated"}`), errors.New("exit status 1")
	}
	_, err := NewBdStore("/city", runner).Get("ci-1")
	if err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "getting bead") {
		t.Fatalf("Get err = %v, want a non-NotFound error", err)
	}
}

// An envelope is held to the same bar as the error text: only a bead-level
// miss is ErrNotFound. Callers treat ErrNotFound as confirmed absence (the
// orphan sweep acts on it), so an infrastructure "not found" — a Dolt
// database mid-restart — in the envelope must surface as itself.
func TestBdStoreGetKeepsInfraNotFoundEnvelopesAsErrors(t *testing.T) {
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return []byte(`{"error": "database not found: hq"}`), errors.New("exit status 1")
	}
	_, err := NewBdStore("/city", runner).Get("ci-1")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Get err = %v, want an infrastructure error, not ErrNotFound", err)
	}
}
