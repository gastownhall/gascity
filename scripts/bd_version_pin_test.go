package scripts_test

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/deps"
)

// TestBDVersionPins keeps every independently-edited bd version anchor in
// lockstep, the same way TestDoltVersionPins does for Dolt. Before this test the
// bd floors drifted apart: deps.env BD_VERSION, the init hard-dependency floor
// bdMinVersion, the ready-projection feature floor bdReadyProjectionMinVersion,
// the bd_compatibility config enum, and the install-bd-archive.sh SHA table were
// all hand-edited with no cross-check, so a regression like #3135 (a 1.0.5 flag
// emitted ahead of the pinned 1.0.4 floor) could merge green. This test makes
// deps.env the single source of truth and fails loudly the moment an anchor
// moves without the others.
func TestBDVersionPins(t *testing.T) {
	root := repoRoot(t)
	env := readDotenv(t, filepath.Join(root, "deps.env"))

	bdVersion := env["BD_VERSION"]         // installable default (v-prefixed release tag)
	bdPrev := env["BD_PREV_VERSION"]       // min-supported matrix cell (downloadable)
	bdCurrent := env["BD_CURRENT_VERSION"] // bleeding-edge matrix cell (built from source)
	bdCurrentRef := env["BD_CURRENT_REF"]  // beads commit the current cell builds from

	if bdVersion == "" {
		t.Fatal("deps.env missing BD_VERSION")
	}
	if bdPrev == "" {
		t.Fatal("deps.env missing BD_PREV_VERSION (the minimum-supported contract-matrix cell)")
	}
	if bdCurrent == "" {
		t.Fatal("deps.env missing BD_CURRENT_VERSION (the bleeding-edge contract-matrix cell)")
	}

	// The current cell has no release tarball, so it is built from a pinned beads
	// commit. A non-deterministic ref (branch name, short SHA) would make the cell
	// irreproducible; require a full 40-char commit SHA.
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(bdCurrentRef) {
		t.Fatalf("deps.env BD_CURRENT_REF = %q, want a full 40-char gastownhall/beads commit SHA", bdCurrentRef)
	}
	if !regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`).MatchString(bdCurrent) {
		t.Fatalf("deps.env BD_CURRENT_VERSION = %q, want a semver token", bdCurrent)
	}

	// Anchor roles, kept as distinct contracts so a promotion cannot quietly
	// collapse them:
	//   BD_PREV_VERSION -- the minimum-supported bd (the matrix floor cell).
	//   BD_VERSION      -- the installable default; must be >= the floor.
	// The init hard-dependency floor (bdMinVersion) is the minimum-supported
	// version restated as a Go constant, so it must track BD_PREV_VERSION, not
	// BD_VERSION. Tying it to BD_VERSION would drag the hard floor up the moment
	// BD_VERSION is promoted (e.g. -> v1.0.5) and drop support for the
	// min-supported matrix cell these contract tests exist to keep green.
	bdMin := extractGoStringConst(t, root, "cmd/gc/init_provider_readiness.go", "bdMinVersion")
	if bdMin != strings.TrimPrefix(bdPrev, "v") {
		t.Fatalf("bdMinVersion = %q but deps.env BD_PREV_VERSION = %q (want %q); the init hard floor is the minimum-supported bd and must track BD_PREV_VERSION, not BD_VERSION",
			bdMin, bdPrev, strings.TrimPrefix(bdPrev, "v"))
	}
	// The installable default may move ahead of the floor but never behind it.
	if deps.CompareVersions(bdVersion, bdPrev) < 0 {
		t.Fatalf("deps.env BD_VERSION = %q is older than BD_PREV_VERSION = %q; the installable default must be at least the minimum-supported version",
			bdVersion, bdPrev)
	}

	// The ready-projection feature floor (#3135's regressing surface) must exist
	// and be strictly newer than the init floor, otherwise the gated path is dead
	// for every supported bd. Compare semantically -- the same way the runtime
	// gate in bdstore_ready_projection.go does (deps.CompareVersions) -- so a
	// floor that is merely different from the init floor, including an older one,
	// cannot pass.
	readyFloor := extractGoStringConst(t, root, "internal/beads/bdstore_ready_projection.go", "bdReadyProjectionMinVersion")
	if readyFloor == "" {
		t.Fatal("internal/beads/bdstore_ready_projection.go missing bdReadyProjectionMinVersion const")
	}
	if deps.CompareVersions(readyFloor, bdMin) <= 0 {
		t.Fatalf("bdReadyProjectionMinVersion (%q) must be strictly newer than bdMinVersion (%q); a feature floor at or below the init floor gates nothing", readyFloor, bdMin)
	}

	// The bd_compatibility config enum is the operator-facing mirror of the two
	// floors; both floor values must appear as enum members so they cannot diverge.
	cfg := readFile(t, root, "internal/config/config.go")
	for _, member := range []string{"enum=bd-" + bdMin, "enum=bd-" + readyFloor} {
		if !strings.Contains(cfg, member) {
			t.Fatalf("internal/config/config.go bd_compatibility enum missing %q (floors: init=%s ready=%s)", member, bdMin, readyFloor)
		}
	}

	// Every released bd that the required CI paths install from a tarball must
	// carry a pinned SHA for every os/arch, never the API fallback. That is both
	// the minimum-supported cell (BD_PREV_VERSION) and the installable default
	// (BD_VERSION): they are the same value today but may diverge on a promotion,
	// and main CI installs BD_VERSION directly. Deduplicate when they are equal.
	install := readFile(t, root, ".github/scripts/install-bd-archive.sh")
	requiredReleases := []string{bdPrev}
	if bdVersion != bdPrev {
		requiredReleases = append(requiredReleases, bdVersion)
	}
	for _, release := range requiredReleases {
		for _, tuple := range []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64"} {
			want := release + ":" + tuple
			if !strings.Contains(install, want) {
				t.Fatalf(".github/scripts/install-bd-archive.sh missing SHA pin %q; %s cannot install on the required path without it", want, release)
			}
		}
	}

	// Every workflow that pins BD_VERSION must pin the same value as deps.env, so a
	// bump in one place cannot leave a stale matrix cell behind. Validate every
	// assignment in both .yml and .yaml workflows: a file-level presence check
	// would let a stale pin ride along beside a correct one.
	assertWorkflowPins(t, root, "BD_VERSION", bdVersion)
}

// TestScanPinAssignments proves the workflow pin scanner catches the partial
// drift a file-level presence check missed: a stale BD_VERSION sharing a file
// with a correct one is still reported with its line, while a
// `${{ env.BD_VERSION }}` reference is not treated as an assignment.
func TestScanPinAssignments(t *testing.T) {
	const fixture = `env:
  BD_VERSION: "v1.0.4"
  DOLT_VERSION: "2.1.7"
jobs:
  stale:
    env:
      BD_VERSION: "v1.0.3"
    steps:
      - with:
          bd-version: ${{ env.BD_VERSION }}
`
	got := scanPinAssignments("BD_VERSION", fixture)
	want := []pinAssignment{
		{line: 2, value: "v1.0.4"},
		{line: 7, value: "v1.0.3"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scanPinAssignments(BD_VERSION) = %+v, want %+v", got, want)
	}
}

// readDotenv parses simple KEY=VALUE lines, ignoring comments and blanks.
func readDotenv(t *testing.T, path string) map[string]string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(content)
}

// extractGoStringConst returns the value of a `name = "..."` Go string constant,
// or "" if the file does not declare it. The pattern is anchored to a real
// declaration form -- the identifier must start a line, optionally preceded by
// indentation and the `const` keyword -- so a comment or prose example naming the
// same identifier above the real const cannot be matched first.
func extractGoStringConst(t *testing.T, root, rel, name string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^\s*(?:const\s+)?` + regexp.QuoteMeta(name) + `\s*=\s*"([^"]+)"`)
	m := re.FindStringSubmatch(readFile(t, root, rel))
	if m == nil {
		return ""
	}
	return m[1]
}

// pinAssignment is a single `KEY: value` mapping entry found in a workflow file,
// carrying its 1-based line number for diagnostics.
type pinAssignment struct {
	line  int
	value string
}

// scanPinAssignments returns every `key: value` assignment in content. It matches
// only a mapping key -- optional indentation, the exact key, then a colon -- so a
// reference such as `bd-version: ${{ env.BD_VERSION }}` is not mistaken for an
// assignment of BD_VERSION. Surrounding quotes and any trailing comment are
// stripped from the captured value.
func scanPinAssignments(key, content string) []pinAssignment {
	re := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(key) + `:\s*["']?([^"'\s#]+)["']?`)
	var out []pinAssignment
	for i, line := range strings.Split(content, "\n") {
		if m := re.FindStringSubmatch(line); m != nil {
			out = append(out, pinAssignment{line: i + 1, value: m[1]})
		}
	}
	return out
}

// assertWorkflowPins fails for every workflow assignment of key whose value is not
// want, scanning both .yml and .yaml workflows and reporting each offending file
// and line. Validating every assignment -- not just file-level presence -- catches
// a file that mixes a correct pin with a stale one, and reporting via t.Errorf
// rather than t.Fatalf surfaces all stale pins in a single run.
func assertWorkflowPins(t *testing.T, root, key, want string) {
	t.Helper()
	dir := filepath.Join(root, ".github", "workflows")
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if ext := filepath.Ext(path); ext != ".yml" && ext != ".yaml" {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, a := range scanPinAssignments(key, string(content)) {
			if a.value != want {
				t.Errorf("%s:%d pins %s to %q, want %q (deps.env)", rel, a.line, key, a.value, want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk workflows: %v", err)
	}
}
