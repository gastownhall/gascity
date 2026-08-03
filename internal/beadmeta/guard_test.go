package beadmeta

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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

	// Cobra command-tree annotations (not bead metadata).
	"gc.docgen.skip":                "cobra annotation: skip CLI doc generation",
	"gc.json.schema_dir":            "cobra annotation: JSON schema output dir",
	"gc.productmetrics.census":      "testhook cobra annotation: omit a synthetic command from the production census",
	"gc.productmetrics.class":       "cobra annotation: closed product-metrics command classification",
	"gc.productmetrics.conditional": "cobra annotation: product-metrics conditional policy",
	"gc.productmetrics.exclusion":   "cobra annotation: product-metrics exclusion reason",
	"gc.productmetrics.id":          "cobra annotation: stable product-metrics command ID",
	"gc.productmetrics.mode":        "cobra annotation: product-metrics command handling mode",
	"gc.productmetrics.notice":      "cobra annotation: product-metrics notice policy",
	"gc.productmetrics.owner":       "cobra annotation: product-metrics command owner",
	"gc.productmetrics.recording":   "cobra annotation: product-metrics recording policy",
	"gc.productmetrics.resolver":    "cobra annotation: product-metrics dynamic resolver",

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
	for _, rel := range trackedGoFiles(t, root, []string{"internal", "cmd"}) {
		relSlash := filepath.ToSlash(rel)
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if perr != nil {
			continue // unparseable file is not this guard's concern
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
				violations = append(violations, fmt.Sprintf("  %s:%d  %q is declared — reference the beadmeta constant instead of the raw literal", relSlash, line, val))
			} else {
				violations = append(violations, fmt.Sprintf("  %s:%d  %q is undeclared — declare it in internal/beadmeta/keys.go", relSlash, line, val))
			}
			return true
		})
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

// TestTrackedGoFilesExcludesUntrackedScaffoldNoise locks in defense against
// the same scaffold-noise class that tripped internal/api/apierr_guard_test.go
// before PR#4118: a raw filepath.WalkDir over the repo tree descends into
// whatever happens to be sitting on disk, including stray ga-* bead-worktree
// checkouts and .gascity-worktree-stage.* staging dirs that a concurrent
// fleet agent may have left under repo root. trackedGoFiles must scan
// git-tracked files only, so untracked scaffold noise can never be walked,
// regardless of its name. See ga-5vzfgb.
func TestTrackedGoFilesExcludesUntrackedScaffoldNoise(t *testing.T) {
	root := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.invalid",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.invalid",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	writeFile := func(rel, content string) {
		t.Helper()
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	runGit("init", "-q")

	// A real, git-tracked file under internal/ — must always be scanned.
	writeFile("internal/example/tracked.go", "package example\n\nconst x = \"tracked-marker\"\n")
	runGit("add", "internal/example/tracked.go")
	runGit("commit", "-q", "-m", "tracked")

	// Untracked nested ga-*-named worktree-shaped scaffold dir — must never
	// be scanned, regardless of the fact that its name matches a bead id.
	writeFile("internal/example/ga-9zzzzz-stray/scaffold.go", "package stray\n\nconst y = \"scaffold-marker\"\n")

	// Untracked .gascity-worktree-stage.* staging dir — must never be scanned.
	writeFile("internal/example/.gascity-worktree-stage.abc/scaffold.go", "package stage\n\nconst z = \"stage-marker\"\n")

	got := trackedGoFiles(t, root, []string{"internal", "cmd"})
	for i, f := range got {
		got[i] = filepath.ToSlash(f)
	}

	if !slices.Contains(got, "internal/example/tracked.go") {
		t.Fatalf("trackedGoFiles = %v, want to contain the tracked file", got)
	}
	for _, unwanted := range []string{
		"internal/example/ga-9zzzzz-stray/scaffold.go",
		"internal/example/.gascity-worktree-stage.abc/scaffold.go",
	} {
		if slices.Contains(got, unwanted) {
			t.Fatalf("trackedGoFiles = %v must NOT contain untracked scaffold file %q", got, unwanted)
		}
	}
}

// trackedGoFiles returns the repo-relative paths of every git-tracked,
// non-test .go file under any of the given top-level directories (each
// checked against excludedDirs and a testdata skip, matching the semantics
// filepath.WalkDir previously enforced during the walk itself), using
// `git ls-files` instead of a filesystem walk. This is immune by
// construction to untracked scaffold noise landing under root — a stray
// ga-* bead-worktree checkout or .gascity-worktree-stage.* staging dir is
// never git-tracked, so it can never appear in the result, regardless of
// its name. Mirrors internal/api/apierr_guard_test.go (PR#4118).
func trackedGoFiles(t *testing.T, root string, tops []string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files in %s: %v", root, err)
	}

	var files []string
	for _, rel := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if rel == "" || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		relSlash := filepath.ToSlash(rel)
		inScope := false
		for _, top := range tops {
			if relSlash == top || strings.HasPrefix(relSlash, top+"/") {
				inScope = true
				break
			}
		}
		if !inScope {
			continue
		}
		if strings.HasPrefix(relSlash, "testdata/") || strings.Contains(relSlash, "/testdata/") {
			continue
		}
		if isExcludedDir(filepath.ToSlash(filepath.Dir(relSlash))) {
			continue
		}
		files = append(files, rel)
	}
	return files
}
