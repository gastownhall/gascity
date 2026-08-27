package scripts_test

import (
	"fmt"
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
	// The native Go store, the bleeding-edge contract-matrix cell, and the
	// source-built agent image must all use the same beads commit. A drift here
	// can pair one schema catalog with another version's write behavior. This is
	// the only go.mod reading of the pin: a second, replace-blind copy would keep
	// asserting the require line on a fork that replaces beads, and go red on the
	// very ref the build actually links.
	if err := validateBeadsPseudoVersionPin(bdCurrentRef, readFile(t, root, "go.mod")); err != nil {
		t.Fatalf("beads source pin drift: %v", err)
	}
	if !regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`).MatchString(bdCurrent) {
		t.Fatalf("deps.env BD_CURRENT_VERSION = %q, want a semver token", bdCurrent)
	}
	dockerfile := readFile(t, root, "contrib/k8s/Dockerfile.agent")
	if !strings.Contains(dockerfile, "ARG BD_SOURCE_REF="+bdCurrentRef) {
		t.Fatalf("contrib/k8s/Dockerfile.agent BD_SOURCE_REF must equal deps.env BD_CURRENT_REF (%s)", bdCurrentRef)
	}
	if !strings.Contains(dockerfile, "ARG BD_BUILD="+bdCurrentRef[:10]) {
		t.Fatalf("contrib/k8s/Dockerfile.agent BD_BUILD must equal the first 10 characters of BD_CURRENT_REF (%s)", bdCurrentRef[:10])
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

	// The devcontainer README restates the installed version in prose, which
	// makes it an anchor like any other -- and it was the only one no test read,
	// so it sat at v1.0.4 through the promotion to v1.1.0. A doc anchor nothing
	// asserts is how the next bump goes half-applied.
	assertDocPinAnchor(t, root, ".devcontainer/README.md", "BD_VERSION", bdVersion)
}

// assertDocPinAnchor fails when a doc restates a deps.env pin as
// "`KEY` from `deps.env` (currently VALUE)" and VALUE has drifted. The phrasing
// is the contract: prose that names the variable without restating the value is
// not an anchor and is not matched, so a doc can always opt out by dropping the
// parenthetical rather than by going stale.
func assertDocPinAnchor(t *testing.T, root, rel, key, want string) {
	t.Helper()
	re := regexp.MustCompile("`" + regexp.QuoteMeta(key) + "` from `deps\\.env` \\(currently ([^)]+)\\)")
	m := re.FindStringSubmatch(readFile(t, root, rel))
	if m == nil {
		t.Fatalf("%s no longer restates %s as \"`%s` from `deps.env` (currently <value>)\"; either restore that phrasing or drop this assertion with the anchor", rel, key, key)
	}
	if got := strings.TrimSpace(m[1]); got != want {
		t.Errorf("%s says %s is currently %q, want %q (deps.env)", rel, key, got, want)
	}
}

func TestBeadsPseudoVersionMatchesCurrentRef(t *testing.T) {
	const goMod = `module example.com/fixture

require github.com/steveyegge/beads v1.1.1-0.20260729081659-0123456789ab
`
	const currentRef = "0123456789abcdef0123456789abcdef01234567"
	if err := validateBeadsPseudoVersionPin(currentRef, goMod); err != nil {
		t.Fatalf("validate matching beads pseudo-version pin: %v", err)
	}
}

func TestBeadsPseudoVersionRejectsDifferentCurrentRef(t *testing.T) {
	const goMod = `module example.com/fixture

require github.com/steveyegge/beads v1.1.1-0.20260729081659-0123456789ab
`
	err := validateBeadsPseudoVersionPin("fedcba9876543210fedcba9876543210fedcba98", goMod)
	if err == nil || !strings.Contains(err.Error(), "0123456789ab") {
		t.Fatalf("validate mismatched beads pseudo-version pin error = %v, want embedded commit diagnostic", err)
	}
}

// TestBeadsPseudoVersionPrefersReplaceTarget proves the pin follows a go.mod
// `replace`. Once beads is replaced the require-line pseudo-version is inert —
// nothing builds it — so BD_CURRENT_REF must describe the replacement.
func TestBeadsPseudoVersionPrefersReplaceTarget(t *testing.T) {
	const goMod = `module example.com/fixture

require github.com/steveyegge/beads v1.1.1-0.20260805093327-0123456789ab

replace github.com/steveyegge/beads => example.com/beads-fork v0.0.0-20260813154229-fedcba987654
`
	if err := validateBeadsPseudoVersionPin("fedcba987654321000000000000000000000abcd", goMod); err != nil {
		t.Fatalf("validate replace-target pin: %v", err)
	}
}

// TestBeadsPseudoVersionRejectsRequireLineWhenReplaced is the control for the
// test above: it uses the require line's own commit, which the pre-replace
// check accepted. Following the replace means that ref is now stale, and the
// diagnostic must name the replacement commit so the fix is obvious.
func TestBeadsPseudoVersionRejectsRequireLineWhenReplaced(t *testing.T) {
	const goMod = `module example.com/fixture

require github.com/steveyegge/beads v1.1.1-0.20260805093327-0123456789ab

replace github.com/steveyegge/beads => example.com/beads-fork v0.0.0-20260813154229-fedcba987654
`
	err := validateBeadsPseudoVersionPin("0123456789abcdef0123456789abcdef01234567", goMod)
	if err == nil || !strings.Contains(err.Error(), "fedcba987654") {
		t.Fatalf("validate require-line ref against a replaced beads = %v, want a diagnostic naming the replacement commit", err)
	}
}

// TestBeadsPseudoVersionReadsBlockFormReplace covers the parenthesized replace
// block, which carries no `replace` keyword on the directive line.
func TestBeadsPseudoVersionReadsBlockFormReplace(t *testing.T) {
	const goMod = `module example.com/fixture

require github.com/steveyegge/beads v1.1.1-0.20260805093327-0123456789ab

replace (
	example.com/unrelated => example.com/other v1.2.3
	github.com/steveyegge/beads v1.1.1 => example.com/beads-fork v0.0.0-20260813154229-fedcba987654 // fork
)
`
	if err := validateBeadsPseudoVersionPin("fedcba987654321000000000000000000000abcd", goMod); err != nil {
		t.Fatalf("validate block-form replace-target pin: %v", err)
	}
}

// TestBeadsPseudoVersionRejectsReplaceWithoutCommit refuses to fall back to the
// require line when the replacement carries no commit (a released tag, a local
// path). Falling back would assert a pin that nothing builds — a silent false
// green — so the contradiction is reported instead.
func TestBeadsPseudoVersionRejectsReplaceWithoutCommit(t *testing.T) {
	const goMod = `module example.com/fixture

require github.com/steveyegge/beads v1.1.1-0.20260805093327-0123456789ab

replace github.com/steveyegge/beads => example.com/beads-fork v1.2.3
`
	err := validateBeadsPseudoVersionPin("0123456789abcdef0123456789abcdef01234567", goMod)
	if err == nil || !strings.Contains(err.Error(), "v1.2.3") {
		t.Fatalf("validate against a tag-replaced beads = %v, want a refusal naming the replace target", err)
	}
}

// beadsPseudoVersionCommit extracts the 12-character source commit a Go
// pseudo-version embeds. Every pseudo-version ends in "<sep><14-digit UTC
// timestamp>-<12-char commit>", so one pattern covers the untagged form
// (v0.0.0-<ts>-<commit>) and the tagged forms (v1.1.1-0.<ts>-<commit>,
// v1.1.0-rc.1.0.<ts>-<commit>) alike.
var beadsPseudoVersionCommit = regexp.MustCompile(`^v\S*[-.]\d{14}-([0-9a-f]{12})$`)

// beadsRequirePin captures the version token on the go.mod require line for
// beads.
var beadsRequirePin = regexp.MustCompile(`(?m)^\s*(?:require\s+)?github\.com/steveyegge/beads\s+(v\S+)`)

// beadsReplaceTarget returns the right-hand side of a go.mod `replace` for
// github.com/steveyegge/beads, or "" when the module is not replaced. It reads
// both the single-line form and the parenthesized block form, whose directive
// lines carry no `replace` keyword, and strips any trailing `// comment`.
func beadsReplaceTarget(goMod string) string {
	inBlock := false
	for _, line := range strings.Split(goMod, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "replace ("):
			inBlock = true
			continue
		case inBlock && line == ")":
			inBlock = false
			continue
		case strings.HasPrefix(line, "replace "):
			line = strings.TrimSpace(strings.TrimPrefix(line, "replace"))
		case !inBlock:
			continue
		}
		lhs, rhs, ok := strings.Cut(line, "=>")
		if !ok {
			continue
		}
		fields := strings.Fields(lhs)
		if len(fields) == 0 || fields[0] != "github.com/steveyegge/beads" {
			continue
		}
		return strings.TrimSpace(rhs)
	}
	return ""
}

// beadsLinkedCommit returns the 12-character beads commit the module graph
// actually links, plus a description of where it was read from. A `replace`
// wins over the require line: after a replace the require-line pseudo-version
// is inert, so validating BD_CURRENT_REF against it would pin a version the
// build never uses.
func beadsLinkedCommit(goMod string) (commit, source string, err error) {
	if target := beadsReplaceTarget(goMod); target != "" {
		fields := strings.Fields(target)
		version := ""
		if len(fields) > 0 {
			version = fields[len(fields)-1]
		}
		match := beadsPseudoVersionCommit.FindStringSubmatch(version)
		if match == nil {
			return "", "", fmt.Errorf("go.mod replaces github.com/steveyegge/beads with %q, which embeds no pseudo-version commit; deps.env BD_CURRENT_REF names a commit and cannot be kept in lockstep with it", target)
		}
		return match[1], fmt.Sprintf("the go.mod replace target %q", target), nil
	}
	require := beadsRequirePin.FindStringSubmatch(goMod)
	if require == nil {
		return "", "", fmt.Errorf("go.mod does not require github.com/steveyegge/beads")
	}
	match := beadsPseudoVersionCommit.FindStringSubmatch(require[1])
	if match == nil {
		return "", "", fmt.Errorf("go.mod does not contain a github.com/steveyegge/beads pseudo-version with an embedded 12-character commit")
	}
	return match[1], "the go.mod require line", nil
}

// validateBeadsPseudoVersionPin verifies that the Go decoder and the current
// real-bd matrix binary are built from the same beads commit. Go pseudo-versions
// retain the first 12 characters of the source commit, while deps.env records
// the reproducible full 40-character ref.
func validateBeadsPseudoVersionPin(currentRef, goMod string) error {
	commit, source, err := beadsLinkedCommit(goMod)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(currentRef, commit) {
		return fmt.Errorf("deps.env BD_CURRENT_REF %q does not match github.com/steveyegge/beads pseudo-version commit %q from %s", currentRef, commit, source)
	}
	return nil
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
