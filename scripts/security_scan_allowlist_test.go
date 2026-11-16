package scripts_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"
)

// securityScanAllowlistPath holds the waivers security-scan.yml's gate
// (.github/workflows/scripts/security_scan_gate.py) applies.
const securityScanAllowlistPath = ".github/security-scan-allowlist.toml"

// scanAllowlistEntry is one decoded waiver: its id, its expiry as YYYY-MM-DD
// ("" when missing), its reason, and the source lock, package and version it
// accepts the advisory in.
type scanAllowlistEntry struct {
	id, until, reason, source, pkg, version string
}

func decodeSecurityScanAllowlist(t *testing.T, body string) []scanAllowlistEntry {
	t.Helper()
	var raw struct {
		IgnoredVulns []map[string]any `toml:"IgnoredVulns"`
	}
	if _, err := toml.Decode(body, &raw); err != nil {
		t.Fatalf("parsing %s: %v", securityScanAllowlistPath, err)
	}
	entries := make([]scanAllowlistEntry, 0, len(raw.IgnoredVulns))
	for _, vuln := range raw.IgnoredVulns {
		entry := scanAllowlistEntry{}
		entry.id, _ = vuln["id"].(string)
		entry.reason, _ = vuln["reason"].(string)
		entry.source, _ = vuln["source"].(string)
		entry.pkg, _ = vuln["package"].(string)
		entry.version, _ = vuln["version"].(string)
		if until, ok := vuln["ignoreUntil"]; ok {
			if stamp, ok := until.(interface{ Format(string) string }); ok {
				entry.until = stamp.Format("2006-01-02")
			}
		}
		entries = append(entries, entry)
	}
	return entries
}

// trivyPathsNameRelease reports whether one of a .trivyignore.yaml waiver's
// paths is in the dist-info directory of dist at version, the way each
// gc-mcp-mail waiver names the release it accepts.
func trivyPathsNameRelease(paths []string, dist, version string) bool {
	want := strings.ToLower("/" + dist + "-" + version + ".dist-info/")
	for _, p := range paths {
		if strings.Contains(strings.ToLower("/"+p), want) {
			return true
		}
	}
	return false
}

// allowlistMirrorErrors reports every allowlist entry that is not a
// time-boxed, explained mirror of a .trivyignore.yaml waiver for the same
// release, and every id waived twice. An entry mirrors its waiver when the
// ids and expiry dates match, the entry's source lock (from locks, keyed by
// repo-relative path) pins its package at its version, and one of the
// waiver's paths names that same release.
func allowlistMirrorErrors(entries []scanAllowlistEntry, trivy trivyIgnoreDoc, locks map[string]string) []string {
	type trivyWaiver struct {
		expiry string
		paths  []string
	}
	trivyWaivers := map[string]trivyWaiver{}
	for _, waiver := range trivy.Vulnerabilities {
		trivyWaivers[waiver.ID] = trivyWaiver{expiry: waiver.ExpiredAt, paths: waiver.Paths}
	}
	var errs []string
	seen := map[string]bool{}
	for _, entry := range entries {
		if strings.TrimSpace(entry.id) == "" {
			errs = append(errs, securityScanAllowlistPath+": an [[IgnoredVulns]] entry has no id")
			continue
		}
		where := securityScanAllowlistPath + ": " + entry.id
		if seen[entry.id] {
			errs = append(errs, where+" is listed twice")
		}
		seen[entry.id] = true
		if entry.until == "" {
			errs = append(errs, where+" has no ignoreUntil date; every waiver expires")
		}
		if strings.TrimSpace(entry.reason) == "" {
			errs = append(errs, where+" has no reason")
		}
		release := entry.pkg + " " + entry.version
		named := entry.source != "" && entry.pkg != "" && entry.version != ""
		if !named {
			errs = append(errs, where+" names no source, package and version; a waiver accepts one release in one lock")
		} else if lock, ok := locks[entry.source]; !ok {
			errs = append(errs, where+" names source "+entry.source+", which is not a lock file in the repo")
		} else if pinned, ok := lockPin(lock, entry.pkg); !ok {
			errs = append(errs, where+" accepts "+release+", but "+entry.source+" does not pin "+entry.pkg)
		} else if pinned != entry.version {
			errs = append(errs, where+" accepts "+release+", but "+entry.source+" pins "+pinned)
		}
		waiver, ok := trivyWaivers[entry.id]
		switch {
		case !ok:
			errs = append(errs, where+" mirrors no .trivyignore.yaml waiver; document the risk there first")
			continue
		case entry.until != "" && waiver.expiry != entry.until:
			errs = append(errs, where+" expires "+entry.until+" but its .trivyignore.yaml waiver expires "+waiver.expiry)
		}
		if named && !trivyPathsNameRelease(waiver.paths, entry.pkg, entry.version) {
			errs = append(errs, where+" accepts "+release+", but its .trivyignore.yaml waiver names no "+entry.pkg+"-"+entry.version+".dist-info path; both must accept the same release")
		}
	}
	return errs
}

// TestSecurityScanAllowlistMirrorsTrivyWaivers keeps the security scan's
// allowlist a strict subset of the repo's documented waivers: a risk is
// accepted once, in .trivyignore.yaml, with its rationale, and the weekly
// dependency scan accepts it only for the release the lock pins and the image
// scan waives, and stops accepting it on the same day the image scan does.
func TestSecurityScanAllowlistMirrorsTrivyWaivers(t *testing.T) {
	root := repoRoot(t)
	var trivy trivyIgnoreDoc
	if err := yaml.Unmarshal([]byte(readFile(t, root, ".trivyignore.yaml")), &trivy); err != nil {
		t.Fatalf("parsing .trivyignore.yaml: %v", err)
	}
	entries := decodeSecurityScanAllowlist(t, readFile(t, root, securityScanAllowlistPath))
	if len(entries) == 0 {
		t.Fatalf("%s has no [[IgnoredVulns]] entries; the gc-mcp-mail lock waivers belong there", securityScanAllowlistPath)
	}
	locks := map[string]string{}
	for _, entry := range entries {
		if entry.source == "" || !filepath.IsLocal(filepath.FromSlash(entry.source)) {
			continue
		}
		if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(entry.source))); err == nil && info.Mode().IsRegular() {
			locks[entry.source] = readFile(t, root, entry.source)
		}
	}
	for _, err := range allowlistMirrorErrors(entries, trivy, locks) {
		t.Error(err)
	}
}

func TestSecurityScanAllowlistMirrorRejectsDrift(t *testing.T) {
	var trivy trivyIgnoreDoc
	for id, dist := range map[string]string{"CVE-2026-1": "gitpython-3.1.62", "CVE-2026-2": "gitpython-3.1.62", "CVE-2026-3": "requests-2.0.0"} {
		trivy.Vulnerabilities = append(trivy.Vulnerabilities,
			struct {
				ID        string   `yaml:"id"`
				Paths     []string `yaml:"paths"`
				Purls     []string `yaml:"purls"`
				ExpiredAt string   `yaml:"expired_at"`
				Statement string   `yaml:"statement"`
			}{ID: id, Paths: []string{mcpMailSitePackages + dist + ".dist-info/METADATA"}, ExpiredAt: "2027-01-26"},
		)
	}
	locks := map[string]string{"lock.txt": "filelock==3.29.0 \\\n    --hash=sha256:00\ngitpython==3.1.62 \\\n    --hash=sha256:00\n"}
	entries := decodeSecurityScanAllowlist(t, `
[[IgnoredVulns]]
id = "CVE-2026-1"
ignoreUntil = 2027-01-26
source = "lock.txt"
package = "gitpython"
version = "3.1.62"
reason = "mirrors the trivy waiver"

[[IgnoredVulns]]
id = "CVE-2026-1"
ignoreUntil = 2027-02-26
source = "lock.txt"
package = "gitpython"
version = "3.1.62"
reason = "drifted expiry, listed twice"

[[IgnoredVulns]]
id = "CVE-2026-2"
ignoreUntil = 2027-01-26
source = "lock.txt"
package = "gitpython"
version = "3.1.63"
reason = "a release neither the lock nor the trivy waiver names"

[[IgnoredVulns]]
id = "CVE-2026-3"
ignoreUntil = 2027-01-26
source = "lock.txt"
package = "requests"
version = "2.0.0"
reason = "a package the lock does not pin"

[[IgnoredVulns]]
id = "GHSA-undocumented"
ignoreUntil = 2027-01-26
source = "other.txt"
package = "filelock"
version = "3.29.0"
reason = "no trivy waiver, and a source that is not a lock"

[[IgnoredVulns]]
id = "CVE-2026-1-forever"
reason = ""
`)
	got := allowlistMirrorErrors(entries, trivy, locks)
	want := []string{
		securityScanAllowlistPath + ": CVE-2026-1 is listed twice",
		securityScanAllowlistPath + ": CVE-2026-1 expires 2027-02-26 but its .trivyignore.yaml waiver expires 2027-01-26",
		securityScanAllowlistPath + ": CVE-2026-2 accepts gitpython 3.1.63, but lock.txt pins 3.1.62",
		securityScanAllowlistPath + ": CVE-2026-2 accepts gitpython 3.1.63, but its .trivyignore.yaml waiver names no gitpython-3.1.63.dist-info path; both must accept the same release",
		securityScanAllowlistPath + ": CVE-2026-3 accepts requests 2.0.0, but lock.txt does not pin requests",
		securityScanAllowlistPath + ": GHSA-undocumented names source other.txt, which is not a lock file in the repo",
		securityScanAllowlistPath + ": GHSA-undocumented mirrors no .trivyignore.yaml waiver; document the risk there first",
		securityScanAllowlistPath + ": CVE-2026-1-forever has no ignoreUntil date; every waiver expires",
		securityScanAllowlistPath + ": CVE-2026-1-forever has no reason",
		securityScanAllowlistPath + ": CVE-2026-1-forever names no source, package and version; a waiver accepts one release in one lock",
		securityScanAllowlistPath + ": CVE-2026-1-forever mirrors no .trivyignore.yaml waiver; document the risk there first",
	}
	if !slices.Equal(got, want) {
		t.Errorf("allowlistMirrorErrors =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// TestSecurityScanNamesEveryHashedRequirementsLock: osv-scanner finds
// requirements.txt by name only, so each hashed pip lock elsewhere must be
// handed to it with -L, or the weekly scan silently skips it.
func TestSecurityScanNamesEveryHashedRequirementsLock(t *testing.T) {
	root := repoRoot(t)
	workflow := readFile(t, root, ".github/workflows/security-scan.yml")
	locks, err := filepath.Glob(filepath.Join(root, ".github", "requirements", "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var hashed int
	for _, lock := range locks {
		body, err := os.ReadFile(lock)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "--hash=") {
			continue
		}
		hashed++
		rel, err := filepath.Rel(root, lock)
		if err != nil {
			t.Fatal(err)
		}
		if flag := "-L requirements.txt:" + filepath.ToSlash(rel); !strings.Contains(workflow, flag) {
			t.Errorf("security-scan.yml: osv-scanner does not scan %s; add %q", rel, flag)
		}
	}
	if hashed == 0 {
		t.Fatal("found no hashed lock under .github/requirements; the test is not looking where the locks are")
	}
}

// TestSecurityScanCoversEveryGoModule: every Go module but the root one,
// which govulncheck.yml scans, is in security-scan.yml's govulncheck matrix.
func TestSecurityScanCoversEveryGoModule(t *testing.T) {
	root := repoRoot(t)
	var modules []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() && path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "bazel-") ||
			name == "testdata" || name == "node_modules" || name == "third_party") {
			return filepath.SkipDir
		}
		if !entry.IsDir() && name == "go.mod" {
			dir, err := filepath.Rel(root, filepath.Dir(path))
			if err != nil {
				return err
			}
			if dir != "." {
				modules = append(modules, filepath.ToSlash(dir))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking for go.mod: %v", err)
	}
	sort.Strings(modules)

	var workflow struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					Module []string `yaml:"module"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, root, ".github/workflows/security-scan.yml")), &workflow); err != nil {
		t.Fatalf("parsing security-scan.yml: %v", err)
	}
	matrix := slices.Clone(workflow.Jobs["govulncheck"].Strategy.Matrix.Module)
	sort.Strings(matrix)
	if len(modules) == 0 {
		t.Fatal("found no nested go.mod; tools/nogo should be one")
	}
	if !slices.Equal(matrix, modules) {
		t.Errorf("security-scan.yml govulncheck matrix = %v, want every nested Go module %v", matrix, modules)
	}
}
