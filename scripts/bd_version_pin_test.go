package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
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

	// The init hard-dependency floor must equal BD_VERSION (modulo the v prefix);
	// these are the same fact stated in two languages (shell pin vs Go constant).
	bdMin := extractGoStringConst(t, root, "cmd/gc/init_provider_readiness.go", "bdMinVersion")
	if bdMin != strings.TrimPrefix(bdVersion, "v") {
		t.Fatalf("bdMinVersion = %q but deps.env BD_VERSION = %q (want %q); update both together",
			bdMin, bdVersion, strings.TrimPrefix(bdVersion, "v"))
	}

	// The ready-projection feature floor (#3135's regressing surface) must exist and
	// be strictly newer than the init floor, otherwise the gated path is dead.
	readyFloor := extractGoStringConst(t, root, "internal/beads/bdstore_ready_projection.go", "bdReadyProjectionMinVersion")
	if readyFloor == "" {
		t.Fatal("internal/beads/bdstore_ready_projection.go missing bdReadyProjectionMinVersion const")
	}
	if readyFloor == bdMin {
		t.Fatalf("bdReadyProjectionMinVersion (%q) must be newer than bdMinVersion (%q); a feature floor equal to the init floor gates nothing", readyFloor, bdMin)
	}

	// The bd_compatibility config enum is the operator-facing mirror of the two
	// floors; both floor values must appear as enum members so they cannot diverge.
	cfg := readFile(t, root, "internal/config/config.go")
	for _, member := range []string{"enum=bd-" + bdMin, "enum=bd-" + readyFloor} {
		if !strings.Contains(cfg, member) {
			t.Fatalf("internal/config/config.go bd_compatibility enum missing %q (floors: init=%s ready=%s)", member, bdMin, readyFloor)
		}
	}

	// The minimum-supported matrix cell must install from a pinned SHA, never the
	// API fallback. Require an explicit case-table entry for every os/arch.
	install := readFile(t, root, ".github/scripts/install-bd-archive.sh")
	for _, tuple := range []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64"} {
		want := bdPrev + ":" + tuple
		if !strings.Contains(install, want) {
			t.Fatalf(".github/scripts/install-bd-archive.sh missing SHA pin %q; BD_PREV_VERSION cannot install on the required path without it", want)
		}
	}

	// Every workflow that pins BD_VERSION must pin the same value as deps.env, so a
	// bump in one place cannot leave a stale matrix cell behind.
	walkWorkflows(t, root, func(rel, content string) {
		if strings.Contains(content, "BD_VERSION:") &&
			!strings.Contains(content, `BD_VERSION: "`+bdVersion+`"`) {
			t.Fatalf("%s pins BD_VERSION but not to %s (deps.env)", rel, bdVersion)
		}
	})
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
// or "" if the file does not declare it.
func extractGoStringConst(t *testing.T, root, rel, name string) string {
	t.Helper()
	re := regexp.MustCompile(regexp.QuoteMeta(name) + `\s*=\s*"([^"]+)"`)
	m := re.FindStringSubmatch(readFile(t, root, rel))
	if m == nil {
		return ""
	}
	return m[1]
}

func walkWorkflows(t *testing.T, root string, check func(rel, content string)) {
	t.Helper()
	dir := filepath.Join(root, ".github", "workflows")
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".yml" {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		check(rel, string(content))
		return nil
	})
	if err != nil {
		t.Fatalf("walk workflows: %v", err)
	}
}
