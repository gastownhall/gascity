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

// osvAllowlistEntry is one decoded waiver: its id, its expiry as YYYY-MM-DD
// ("" when missing) and its reason.
type osvAllowlistEntry struct {
	id, until, reason string
}

func decodeOSVScannerAllowlist(t *testing.T, body string) []osvAllowlistEntry {
	t.Helper()
	var raw struct {
		IgnoredVulns []map[string]any `toml:"IgnoredVulns"`
	}
	if _, err := toml.Decode(body, &raw); err != nil {
		t.Fatalf("parsing osv-scanner.toml: %v", err)
	}
	entries := make([]osvAllowlistEntry, 0, len(raw.IgnoredVulns))
	for _, vuln := range raw.IgnoredVulns {
		entry := osvAllowlistEntry{}
		entry.id, _ = vuln["id"].(string)
		entry.reason, _ = vuln["reason"].(string)
		if until, ok := vuln["ignoreUntil"]; ok {
			if stamp, ok := until.(interface{ Format(string) string }); ok {
				entry.until = stamp.Format("2006-01-02")
			}
		}
		entries = append(entries, entry)
	}
	return entries
}

// allowlistMirrorErrors reports every osv-scanner.toml entry that is not a
// time-boxed, explained mirror of a .trivyignore.yaml waiver with the same id
// and the same expiry date, and every id waived twice.
func allowlistMirrorErrors(entries []osvAllowlistEntry, trivy trivyIgnoreDoc) []string {
	trivyExpiry := map[string]string{}
	for _, waiver := range trivy.Vulnerabilities {
		trivyExpiry[waiver.ID] = waiver.ExpiredAt
	}
	var errs []string
	seen := map[string]bool{}
	for _, entry := range entries {
		switch {
		case strings.TrimSpace(entry.id) == "":
			errs = append(errs, "osv-scanner.toml: an [[IgnoredVulns]] entry has no id")
			continue
		case seen[entry.id]:
			errs = append(errs, "osv-scanner.toml: "+entry.id+" is listed twice")
		}
		seen[entry.id] = true
		if entry.until == "" {
			errs = append(errs, "osv-scanner.toml: "+entry.id+" has no ignoreUntil date; every waiver expires")
		}
		if strings.TrimSpace(entry.reason) == "" {
			errs = append(errs, "osv-scanner.toml: "+entry.id+" has no reason")
		}
		expiry, ok := trivyExpiry[entry.id]
		switch {
		case !ok:
			errs = append(errs, "osv-scanner.toml: "+entry.id+" mirrors no .trivyignore.yaml waiver; document the risk there first")
		case entry.until != "" && expiry != entry.until:
			errs = append(errs, "osv-scanner.toml: "+entry.id+" expires "+entry.until+" but its .trivyignore.yaml waiver expires "+expiry)
		}
	}
	return errs
}

// TestOSVScannerAllowlistMirrorsTrivyWaivers keeps the security scan's
// allowlist a strict subset of the repo's documented waivers: a risk is
// accepted once, in .trivyignore.yaml, with its rationale, and the weekly
// dependency scan stops accepting it on the same day the image scan does.
func TestOSVScannerAllowlistMirrorsTrivyWaivers(t *testing.T) {
	root := repoRoot(t)
	var trivy trivyIgnoreDoc
	if err := yaml.Unmarshal([]byte(readFile(t, root, ".trivyignore.yaml")), &trivy); err != nil {
		t.Fatalf("parsing .trivyignore.yaml: %v", err)
	}
	entries := decodeOSVScannerAllowlist(t, readFile(t, root, "osv-scanner.toml"))
	if len(entries) == 0 {
		t.Fatal("osv-scanner.toml has no [[IgnoredVulns]] entries; the gc-mcp-mail lock waivers belong there")
	}
	for _, err := range allowlistMirrorErrors(entries, trivy) {
		t.Error(err)
	}
}

func TestOSVScannerAllowlistMirrorRejectsDrift(t *testing.T) {
	var trivy trivyIgnoreDoc
	trivy.Vulnerabilities = append(trivy.Vulnerabilities,
		struct {
			ID        string   `yaml:"id"`
			Paths     []string `yaml:"paths"`
			Purls     []string `yaml:"purls"`
			ExpiredAt string   `yaml:"expired_at"`
			Statement string   `yaml:"statement"`
		}{ID: "CVE-2026-1", ExpiredAt: "2027-01-26"},
	)
	entries := decodeOSVScannerAllowlist(t, `
[[IgnoredVulns]]
id = "CVE-2026-1"
ignoreUntil = 2027-01-26
reason = "mirrors the trivy waiver"

[[IgnoredVulns]]
id = "CVE-2026-1"
ignoreUntil = 2027-02-26
reason = "drifted expiry, listed twice"

[[IgnoredVulns]]
id = "GHSA-undocumented"
ignoreUntil = 2027-01-26
reason = "no trivy waiver"

[[IgnoredVulns]]
id = "CVE-2026-1-forever"
reason = ""
`)
	got := allowlistMirrorErrors(entries, trivy)
	want := []string{
		"osv-scanner.toml: CVE-2026-1 is listed twice",
		"osv-scanner.toml: CVE-2026-1 expires 2027-02-26 but its .trivyignore.yaml waiver expires 2027-01-26",
		"osv-scanner.toml: GHSA-undocumented mirrors no .trivyignore.yaml waiver; document the risk there first",
		"osv-scanner.toml: CVE-2026-1-forever has no ignoreUntil date; every waiver expires",
		"osv-scanner.toml: CVE-2026-1-forever has no reason",
		"osv-scanner.toml: CVE-2026-1-forever mirrors no .trivyignore.yaml waiver; document the risk there first",
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
