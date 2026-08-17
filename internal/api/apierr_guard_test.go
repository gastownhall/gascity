package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api/apierr"
)

// urnLiteralRe matches any Gas City error-type URN literal as it would appear in
// source — the prefix plus whatever follows up to the closing string delimiter
// (quote, whitespace, or backtick). It intentionally does NOT constrain the tail
// to kebab-case: a malformed or mis-cased code (e.g. "...:Rogue", "...:2fa") can
// never be registered, so requiring the tail to look well-formed to be seen would
// make the guard silently ignore exactly the typos it exists to catch. A bare
// prefix (empty tail) matches too and fails LookupURN, so a literal
// "urn:gascity:error:" concatenated with a code is caught as well.
var urnLiteralRe = regexp.MustCompile("urn:gascity:error:[^\"\\s`]*")

// TestEveryEmittedErrorCodeIsRegistered is the error-contract analog of
// TestEveryKnownEventTypeHasRegisteredPayload: it guarantees the API cannot ship
// a problem-type URN the registry doesn't know about. Every urn:gascity:error:<x>
// string literal in non-test Go anywhere in the module (internal/, cmd/, pkg/,
// root, …) must resolve via apierr.LookupURN, and the apierr package is the sole
// place allowed to author a URN literal — every other site must mint errors
// through the catalog constructors (which derive the URN from the registry) so
// the type can never drift from a registered code.
func TestEveryEmittedErrorCodeIsRegistered(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Join(filepath.Dir(currentFile), "..", "..")

	// Scan git-tracked Go source, not a filesystem walk. A raw WalkDir over
	// repoRoot descends into nested Gas City runtime state — checked-out worktrees
	// under .gc/ and .worktrees/ — whose historical copies of shipped files
	// legitimately carry pre-migration raw URN literals, so the guard would fail on
	// stale non-shipped source instead of on what the module actually ships.
	// `git ls-files` is the precise definition of shipped source: it excludes
	// untracked worktrees and build output while still seeing every tracked .go
	// under internal/, cmd/, pkg/, the module root, examples/, and so on.
	out, err := exec.Command("git", "-C", repoRoot, "ls-files", "-z", "--", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files in %s: %v", repoRoot, err)
	}
	for _, rel := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if rel == "" || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		// The apierr package is the registry itself: it authors the URN prefix and
		// (in its own docs) sample URNs. It is the one sanctioned definer. Anchor the
		// exact package path at the module root so an unrelated ".../api/apierr/..."
		// directory elsewhere is not accidentally exempted.
		if strings.HasPrefix(filepath.ToSlash(rel), "internal/api/apierr/") {
			continue
		}
		path := filepath.Join(repoRoot, rel)
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}
		for _, urn := range urnLiteralRe.FindAllString(string(data), -1) {
			if _, ok := apierr.LookupURN(urn); !ok {
				t.Errorf("%s contains unregistered error URN %q — register it in internal/api/apierr/catalog.go or mint it through the catalog constructors", path, urn)
			} else {
				t.Errorf("%s authors a raw error URN literal %q — mint the error through the apierr catalog constructor instead so the URN derives from the registry", path, urn)
			}
		}
	}
}

// TestErrorModelSpecProjection locks the two spec artifacts documentProblemTypes
// produces from the registry: the ErrorModel schema carries the machine `code`
// property, and the x-gascity-problem-types extension is exactly the sorted set
// of registered URNs. This is what keeps the published contract in lockstep with
// the catalog.
func TestErrorModelSpecProjection(t *testing.T) {
	sm := NewSupervisorMux(emptyRoundtripResolver{}, nil, false, "", "", time.Time{})
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json = %d: %s", rec.Code, rec.Body.String())
	}

	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Extensions map[string]json.RawMessage `json:"-"`
					Examples   []any                      `json:"examples"`
				} `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	errorModel, ok := spec.Components.Schemas["ErrorModel"]
	if !ok {
		t.Fatal("spec is missing the ErrorModel schema")
	}
	if _, ok := errorModel.Properties["code"]; !ok {
		t.Fatal("ErrorModel schema is missing the machine `code` property")
	}

	// x-gascity-problem-types must equal the sorted registry URNs. Re-parse the
	// raw type-property object to read the extension (Huma inlines x- extensions
	// as sibling keys on the schema object).
	var rawSpec struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]map[string]any `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rawSpec); err != nil {
		t.Fatalf("parse spec (raw): %v", err)
	}
	typeProp := rawSpec.Components.Schemas["ErrorModel"].Properties["type"]
	got, _ := typeProp["x-gascity-problem-types"].([]any)
	var gotURNs []string
	for _, v := range got {
		if s, ok := v.(string); ok {
			gotURNs = append(gotURNs, s)
		}
	}

	var wantURNs []string
	for _, pt := range apierr.Registered() {
		wantURNs = append(wantURNs, pt.URN())
	}
	if !reflect.DeepEqual(gotURNs, wantURNs) {
		t.Fatalf("x-gascity-problem-types mismatch:\n got=%v\nwant=%v", gotURNs, wantURNs)
	}
}
