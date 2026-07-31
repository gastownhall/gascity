package cityparity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// manifestPath is the one immutable parity manifest. There is exactly one, and
// it is data rather than code so a reviewer can diff what was certified.
const manifestPath = "manifest.json"

// certifiedPackages are the sources whose digests the parity claim rests on:
// the three adapters it compares and the certification itself. A change to any
// of them is a change to the claim.
var certifiedPackages = []string{
	"cityartifact",
	"cityinference",
	"cityneutral",
	"cityparity",
}

// Landing records the commit each adapter landed on, as reported by the branch
// that was merged to produce this certification. It is evidence, not a
// verifier: nothing in a test can prove a commit is still the tip.
type Landing struct {
	Task   string `json:"task"`
	PR     int    `json:"pr"`
	Commit string `json:"commit"`
}

type Manifest struct {
	Schema string `json:"schema"`
	// GeneratedClient records where the generated client digest lives. It is
	// NOT in this repository: the adapters speak hand-written seams
	// (API interfaces) and the generated client is the Team Server's artifact,
	// so this certification cannot resolve its digest and says so instead of
	// implying it checked one.
	GeneratedClient string            `json:"generated_client"`
	Landings        []Landing         `json:"landings"`
	Packages        map[string]string `json:"packages"`
	Files           map[string]string `json:"files"`
	Findings        []string          `json:"findings"`
}

func digestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // fixed, test-local package paths
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// digests walks the certified packages and returns one entry per Go file plus a
// per-package roll-up. A new file changes the package digest, which is what
// makes "any later change invalidates parity" true for additions and not only
// for edits.
func digests(t *testing.T) (files map[string]string, packages map[string]string) {
	t.Helper()
	files = map[string]string{}
	packages = map[string]string{}
	for _, pkg := range certifiedPackages {
		dir := filepath.Join("..", pkg)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read dir %s: %v", dir, err)
		}
		var names []string
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			names = append(names, e.Name())
		}
		if len(names) == 0 {
			t.Fatalf("certified package %s has no Go files", pkg)
		}
		sort.Strings(names)
		roll := sha256.New()
		for _, name := range names {
			rel := pkg + "/" + name
			d := digestFile(t, filepath.Join(dir, name))
			files[rel] = d
			roll.Write([]byte(rel + "\x00" + d + "\n"))
		}
		packages[pkg] = "sha256:" + hex.EncodeToString(roll.Sum(nil))
	}
	return files, packages
}

func loadManifest(t *testing.T) Manifest {
	t.Helper()
	b, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read %s: %v", manifestPath, err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse %s: %v", manifestPath, err)
	}
	return m
}

// AC3 happy path: one current immutable manifest resolves, and every digest it
// consumed still matches. AC3 edge: it does not, and publication is blocked.
//
// Set GC_PARITY_UPDATE=1 to re-cut the manifest. That is a deliberate act of
// re-certification, not a baseline bump: the parity report published against
// the old manifest no longer describes the code.
func TestParityManifestDigestsMatch(t *testing.T) {
	files, packages := digests(t)

	if os.Getenv("GC_PARITY_UPDATE") == "1" {
		m := loadManifest(t)
		m.Files, m.Packages = files, packages
		b, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			t.Fatalf("marshal manifest: %v", err)
		}
		if err := os.WriteFile(manifestPath, append(b, '\n'), 0o644); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
		t.Log("manifest re-cut; re-run without GC_PARITY_UPDATE to verify")
		return
	}

	m := loadManifest(t)
	for name, want := range m.Files {
		got, ok := files[name]
		if !ok {
			t.Errorf("certified file %s is gone: the manifest is stale and parity is invalid", name)
			continue
		}
		if got != want {
			t.Errorf("certified file %s changed since the manifest was cut:\n want %s\n got  %s", name, want, got)
		}
	}
	for name := range files {
		if _, ok := m.Files[name]; !ok {
			t.Errorf("file %s is not in the manifest: it landed after certification, so parity is unproven for it", name)
		}
	}
	for pkg, want := range m.Packages {
		if got := packages[pkg]; got != want {
			t.Errorf("package %s digest changed: want %s got %s", pkg, want, got)
		}
	}
	if len(m.Packages) != len(certifiedPackages) {
		t.Errorf("manifest covers %d packages, certification covers %d", len(m.Packages), len(certifiedPackages))
	}
}

var commitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// AC3: the manifest names the three adapter commits it was cut over. This is
// the landing evidence; it is recorded, not verified, and the test says which.
func TestParityManifestNamesThreeLandings(t *testing.T) {
	m := loadManifest(t)
	if m.Schema == "" {
		t.Error("manifest has no schema version")
	}
	if len(m.Landings) != 3 {
		t.Fatalf("manifest records %d landings, want the three adapter commits", len(m.Landings))
	}
	seen := map[string]bool{}
	for _, l := range m.Landings {
		if !commitRE.MatchString(l.Commit) {
			t.Errorf("landing %s has no full commit id: %q", l.Task, l.Commit)
		}
		if l.PR == 0 || l.Task == "" {
			t.Errorf("landing %+v is missing its task or PR", l)
		}
		if seen[l.Commit] {
			t.Errorf("landing %s repeats commit %s", l.Task, l.Commit)
		}
		seen[l.Commit] = true
	}
	if m.GeneratedClient == "" {
		t.Error("manifest is silent about the generated client; silence is what this program forbids")
	}
	if len(m.Findings) == 0 {
		t.Error("manifest records no findings section; an empty list must be explicit")
	}
}
