package beads

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// These tests replay the bd contract corpus — the canonicalized golden-JSON
// blobs produced by the beads producer suite (cmd/bd/protocol) and vendored
// under testdata/corpus/ — against gascity's REAL decoder paths. They run
// offline (no bd, no Dolt) so every PR cheaply re-asserts that gascity still
// decodes the bd --json shapes it depends on, and surfaces the v2.0 envelope
// migration the moment bd flips it. Refresh the corpus with `make sync-bd-corpus`
// during a deliberate bd pin bump (see
// engdocs/design/beads-gascity-contract-test-system.md).

const corpusDir = "testdata/corpus"

// rehydrateTimestamps swaps the canonical "<TS>" placeholder for a real RFC3339
// timestamp so blobs decode into bdIssue's time.Time fields. The committed
// corpus stays canonicalized (the byte-stable cross-version diff anchor); only
// the in-memory copy under test is rehydrated.
func rehydrateTimestamps(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte(`"<TS>"`), []byte(`"2026-01-01T00:00:00Z"`))
}

func loadCorpusBlob(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(corpusDir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read corpus blob %s: %v (run `make sync-bd-corpus`)", rel, err)
	}
	return b
}

// TestCorpusFlatShowDecodes proves gascity's list/show decoder parses the
// committed bd show shape into a fully-populated bdIssue, including the nested
// dependency. A bd wire change to any of these fields breaks this offline.
func TestCorpusFlatShowDecodes(t *testing.T) {
	issues, err := parseIssuesTolerant(rehydrateTimestamps(loadCorpusBlob(t, "flat/show.json")))
	if err != nil {
		t.Fatalf("parseIssuesTolerant(flat/show): %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("flat/show: got %d issues, want 1", len(issues))
	}
	got := issues[0]
	if got.ID != "corpus-root" {
		t.Errorf("ID = %q, want corpus-root", got.ID)
	}
	if got.Status != "open" {
		t.Errorf("Status = %q, want open", got.Status)
	}
	if got.IssueType != "feature" {
		t.Errorf("IssueType = %q, want feature", got.IssueType)
	}
	if got.Priority == nil || *got.Priority != 1 {
		t.Errorf("Priority = %v, want 1", got.Priority)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero — the created_at timestamp did not decode")
	}
	if len(got.Dependencies) != 1 || got.Dependencies[0].ID != "corpus-dep" {
		t.Errorf("Dependencies = %+v, want one dep corpus-dep", got.Dependencies)
	}
}

// TestCorpusFlatListReadyDecode confirms the array-returning read commands
// decode cleanly through the same tolerant parser gascity uses in production.
func TestCorpusFlatListReadyDecode(t *testing.T) {
	for _, name := range []string{"flat/list.json", "flat/ready.json"} {
		if _, err := parseIssuesTolerant(rehydrateTimestamps(loadCorpusBlob(t, name))); err != nil {
			t.Errorf("parseIssuesTolerant(%s): %v", name, err)
		}
	}
}

// TestCorpusCreateDecodesSingleIssue covers the create path, which returns a
// bare issue object (not an array) that gascity decodes as a single bdIssue.
func TestCorpusCreateDecodesSingleIssue(t *testing.T) {
	var issue bdIssue
	if err := json.Unmarshal(rehydrateTimestamps(loadCorpusBlob(t, "flat/create_root.json")), &issue); err != nil {
		t.Fatalf("decode flat/create_root: %v", err)
	}
	if issue.ID != "corpus-root" {
		t.Errorf("ID = %q, want corpus-root", issue.ID)
	}
	if issue.Title != "Corpus root issue" {
		t.Errorf("Title = %q, want Corpus root issue", issue.Title)
	}
}

// TestCorpusErrorEnvelopeClassified pins the {error, schema_version} not-found
// envelope to gascity's two classifiers that depend on it.
func TestCorpusErrorEnvelopeClassified(t *testing.T) {
	detail := bdStdoutErrorDetail(loadCorpusBlob(t, "flat/error.json"))
	if detail == "" {
		t.Fatal("bdStdoutErrorDetail returned empty for the not-found error envelope")
	}
	if !isBdNotFound(errors.New(detail)) {
		t.Errorf("isBdNotFound(%q) = false, want true", detail)
	}
}

// TestCorpusV2EnvelopeIsForwardIncompatible documents — as an executable fact —
// that gascity's current decoder does NOT understand the v2.0
// {schema_version, data} envelope. When bd flips BD_JSON_ENVELOPE on by default
// and gascity adds matching support, these expectations flip; the failure is
// the signal to update them as part of the coordinated migration, not a
// surprise in production.
func TestCorpusV2EnvelopeIsForwardIncompatible(t *testing.T) {
	if _, err := parseIssuesTolerant(rehydrateTimestamps(loadCorpusBlob(t, "envelope/show.json"))); err == nil {
		t.Fatal("decoder parsed the v2 {schema_version,data} envelope; gascity gained v2 support — update the forward-compat expectations and the coordination protocol")
	}
	if detail := bdStdoutErrorDetail(loadCorpusBlob(t, "envelope/error.json")); detail != "" {
		t.Fatalf("bdStdoutErrorDetail read the v2 error envelope (%q); gascity gained v2 error support — update expectations", detail)
	}
}

// TestCorpusFieldsAreModeledOrExplicitlyIgnored is the bidirectional drift
// detector. Every field bd emits in a show issue must be either decoded by
// bdIssue or in the explicit ignore set below. A new bd field trips this test,
// forcing a deliberate decision — model it or ignore it with a reason — instead
// of silently dropping a capability.
func TestCorpusFieldsAreModeledOrExplicitlyIgnored(t *testing.T) {
	// bd-emitted fields gascity intentionally does not model. Each carries the
	// reason so a future reader knows the omission is deliberate.
	ignored := map[string]string{
		"comment_count":    "display-only count; gc reads comments via a separate path",
		"created_by":       "provenance; gc does not consume the creator identity",
		"owner":            "bd-internal ownership; gc uses assignee instead",
		"dependency_count": "derivable from dependencies; gc recomputes",
		"dependent_count":  "reverse-dependency count; gc does not consume it",
	}
	known := bdIssueJSONTags(t)

	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(loadCorpusBlob(t, "flat/show.json"), &rows); err != nil {
		t.Fatalf("decode flat/show as rows: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("flat/show is empty")
	}
	for field := range rows[0] {
		if known[field] {
			continue
		}
		if _, ok := ignored[field]; ok {
			continue
		}
		t.Errorf("bd emits field %q that gascity neither decodes (bdIssue) nor explicitly ignores; model it or add it to the ignore set with a reason", field)
	}
}

// bdIssueJSONTags returns the set of json field names bdIssue decodes, derived
// by reflection so it stays in sync as the struct evolves.
func bdIssueJSONTags(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	tp := reflect.TypeOf(bdIssue{})
	for i := 0; i < tp.NumField(); i++ {
		name := strings.Split(tp.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
}
