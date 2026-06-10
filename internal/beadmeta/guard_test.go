package beadmeta

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// keyShape matches a literal that is a whole bead-metadata key and nothing else:
// the gc. namespace followed only by key-body characters. This excludes strings
// that merely begin with a key — log messages ("gc.routed_to backfill ..."), jq
// --metadata-field filter fragments ("gc.run_target="), and YAML renderings
// ("gc.endpoint_status:") — so the guard checks keys, not every gc.-prefixed
// string. Those embedded-key surfaces are a deliberate, separately-tracked
// follow-up (the jq/SQL path slice).
var keyShape = regexp.MustCompile(`^gc\.[A-Za-z0-9_.]+$`)

// allowedNonMetadata lists gc.*-prefixed string literals that appear in
// non-test Go but are NOT bead-metadata keys, so the drift guard must not
// require them to be declared in KnownMetadataKeys. Each entry documents why it
// is a different namespace. This list is the explicit, audited boundary of what
// beadmeta owns; keep it small and justified.
//
// It deliberately does NOT contain pack/prompt-private keys or the t3bridge UI
// namespace — those live in excluded directories or never appear as Go literals,
// so the open world stays open without listing every pack key here.
var allowedNonMetadata = map[string]string{
	// JSON envelope schema-version contract strings (their own per-module
	// owners; versioned independently of the metadata vocabulary).
	"gc.dolt.cleanup.v1":       "dolt cleanup manifest schema version (cmd/gc/cmd_dolt_cleanup.go)",
	"gc.healthz.v1":            "workspace healthz workflow contract (internal/workspacesvc)",
	"gc.worker.conformance.v1": "worker conformance report schema version (internal/worker/workertest)",

	// Cobra command annotations (CLI doc-gen plumbing, not bead metadata).
	"gc.docgen.skip":     "cobra annotation: skip CLI doc generation",
	"gc.json.schema_dir": "cobra annotation: JSON schema output dir",

	// Generated shell-completion filenames, not metadata keys.
	"gc.bash": "shell completion filename (cmd/gc/cmd_shell.go)",
	"gc.fish": "shell completion filename (cmd/gc/cmd_shell.go)",

	// City config YAML keys (config-file rewrite, not bead metadata).
	"gc.endpoint_origin": "city config YAML key (internal/beads/contract/files.go)",
	"gc.endpoint_status": "city config YAML key (internal/beads/contract/files.go)",

	// Bead LABEL value (not a Metadata key) and a test-binary name marker.
	"gc.session": "bead Label value, not a Metadata key (internal/agentutil/pool.go)",
	"gc.test":    "go test binary name marker (cmd/gc/test_guard.go)",
}

// excludedDirs are package directories whose gc.* literals belong to a different
// owner than the bead-metadata vocabulary and are therefore not scanned.
var excludedDirs = []string{
	"internal/beadmeta",         // this package declares the vocabulary
	"internal/events",           // gc.* event-type names (events.KnownEventTypes)
	"internal/telemetry",        // gc.* metric/counter names
	"internal/runtime/t3bridge", // t3bridge UI thread-metadata namespace
	"internal/api/genclient",    // generated client code
}

// TestNoUndeclaredMetadataKeys is the inverted analog of the events package's
// TestEveryKnownEventTypeHasRegisteredPayload: rather than asserting a closed
// declared set is fully registered, it scans non-test Go source and asserts every
// whole gc.*-key-shaped string literal is either covered by a declared open-world
// prefix or in the audited non-metadata allowlist. A literal that spells out a
// DECLARED key is also a violation — reference the beadmeta constant instead, so
// the vocabulary stays compiler-checked. This is the open-world-safe shape —
// pack-private keys (which never appear as Go literals) are never flagged, and
// keys embedded inside larger strings (jq filters, SQL JSON paths, fixture
// documents) are out of scope by the key-shape rule.
func TestNoUndeclaredMetadataKeys(t *testing.T) {
	root := repoRoot(t)

	declared := make(map[string]struct{}, len(KnownMetadataKeys))
	for _, k := range KnownMetadataKeys {
		declared[k] = struct{}{}
	}

	var violations []string
	for _, top := range []string{"internal", "cmd"} {
		base := filepath.Join(root, top)
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if d.Name() == "testdata" || isExcludedDir(rel) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return nil // unparseable file is not this guard's concern
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				val, uerr := strconv.Unquote(lit.Value)
				if uerr != nil {
					return true
				}
				if !keyShape.MatchString(val) {
					return true // not a whole bead-metadata key (bare "gc.", message, filter, ...)
				}
				if hasKnownPrefix(val) {
					return true
				}
				if _, ok := allowedNonMetadata[val]; ok {
					return true
				}
				line := fset.Position(lit.Pos()).Line
				if _, ok := declared[val]; ok {
					violations = append(violations, fmt.Sprintf("  %s:%d  %q is declared — reference the beadmeta constant instead of the raw literal", rel, line, val))
				} else {
					violations = append(violations, fmt.Sprintf("  %s:%d  %q is undeclared — declare it in internal/beadmeta/keys.go", rel, line, val))
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", base, err)
		}
	}

	if len(violations) > 0 {
		t.Fatalf("found %d raw gc.* bead-metadata key literal(s) in non-test Go.\n"+
			"Use the beadmeta constant (declaring it in internal/beadmeta/keys.go and\n"+
			"KnownMetadataKeys if new), or, if the literal is not a bead-metadata key, add\n"+
			"it to allowedNonMetadata with a justification:\n%s",
			len(violations), strings.Join(violations, "\n"))
	}
}

func hasKnownPrefix(val string) bool {
	for _, p := range KnownMetadataPrefixes {
		if strings.HasPrefix(val, p) {
			return true
		}
	}
	return false
}

func isExcludedDir(rel string) bool {
	for _, ex := range excludedDirs {
		if rel == ex || strings.HasPrefix(rel, ex+"/") {
			return true
		}
	}
	return false
}

// repoRoot walks up from the test's working directory to the module root
// (the directory containing go.mod).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate go.mod (module root)")
		}
		dir = parent
	}
}
