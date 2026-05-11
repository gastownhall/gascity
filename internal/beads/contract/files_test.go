package contract

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestConfigHasEndpointAuthority(t *testing.T) {
	cases := []struct {
		name string
		cfg  ConfigState
		want bool
	}{
		{name: "empty", cfg: ConfigState{}, want: false},
		{name: "origin only", cfg: ConfigState{EndpointOrigin: EndpointOriginManagedCity}, want: true},
		{name: "host only", cfg: ConfigState{DoltHost: "db.example.com"}, want: true},
		{name: "port only", cfg: ConfigState{DoltPort: "3307"}, want: true},
		{name: "user only", cfg: ConfigState{DoltUser: "root"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ConfigHasEndpointAuthority(tc.cfg); got != tc.want {
				t.Fatalf("ConfigHasEndpointAuthority() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestScopeHasEndpointAuthority(t *testing.T) {
	fs := fsys.OSFS{}
	scope := t.TempDir()
	if ScopeHasEndpointAuthority(fs, scope) {
		t.Fatal("ScopeHasEndpointAuthority(missing) = true, want false")
	}
	if err := fs.WriteFile(filepath.Join(scope, ".beads", "config.yaml"), []byte(`issue_prefix: gc
`), 0o644); err == nil {
		t.Fatal("write should fail without .beads dir")
	}
	if err := fs.MkdirAll(filepath.Join(scope, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(filepath.Join(scope, ".beads", "config.yaml"), []byte(`issue_prefix: gc
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if ScopeHasEndpointAuthority(fs, scope) {
		t.Fatal("ScopeHasEndpointAuthority(legacy-minimal) = true, want false")
	}
	if err := fs.WriteFile(filepath.Join(scope, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: managed_city
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !ScopeHasEndpointAuthority(fs, scope) {
		t.Fatal("ScopeHasEndpointAuthority(authoritative) = false, want true")
	}
}

func TestIsLegacyMinimalEndpointConfig(t *testing.T) {
	if !IsLegacyMinimalEndpointConfig(ConfigState{}) {
		t.Fatal("IsLegacyMinimalEndpointConfig(empty) = false, want true")
	}
	for _, tc := range []struct {
		name string
		cfg  ConfigState
	}{
		{name: "origin", cfg: ConfigState{EndpointOrigin: EndpointOriginManagedCity}},
		{name: "status", cfg: ConfigState{EndpointStatus: EndpointStatusVerified}},
		{name: "host", cfg: ConfigState{DoltHost: "db.example.com"}},
		{name: "port", cfg: ConfigState{DoltPort: "3307"}},
		{name: "user", cfg: ConfigState{DoltUser: "root"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if IsLegacyMinimalEndpointConfig(tc.cfg) {
				t.Fatalf("IsLegacyMinimalEndpointConfig(%s) = true, want false", tc.name)
			}
		})
	}
}

func TestEnsureCanonicalConfigCreatesManagedShape(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	changed, err := EnsureCanonicalConfig(fs, path, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	})
	if err != nil {
		t.Fatalf("EnsureCanonicalConfig() error = %v", err)
	}
	if !changed {
		t.Fatal("EnsureCanonicalConfig() should report changes for new file")
	}

	data, err := fs.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, needle := range []string{
		"issue_prefix: gc",
		"issue-prefix: gc",
		"dolt.auto-start: false",
		"export.auto: false",
		"gc.endpoint_origin: managed_city",
		"gc.endpoint_status: verified",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("config missing %q:\n%s", needle, text)
		}
	}
	for _, forbidden := range []string{"dolt.host:", "dolt.port:", "dolt.user:"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("config should not contain %q:\n%s", forbidden, text)
		}
	}
}

func TestEnsureCanonicalConfigPreservesUnknownKeysAndScrubsDeprecatedOnes(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	input := strings.Join([]string{
		"custom_key: keepme",
		"issue-prefix: old",
		"dolt.auto-start: true",
		"dolt_server_port: 3307",
		"dolt_port: 4406",
		"dolt.password: should-not-stay",
		"",
	}, "\n")
	if err := fs.WriteFile(path, []byte(input), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := EnsureCanonicalConfig(fs, path, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	})
	if err != nil {
		t.Fatalf("EnsureCanonicalConfig() error = %v", err)
	}
	if !changed {
		t.Fatal("EnsureCanonicalConfig() should report changes")
	}

	data, err := fs.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "custom_key: keepme") {
		t.Fatalf("config should preserve unknown keys:\n%s", text)
	}
	for _, forbidden := range []string{"dolt.password", "dolt_server_port", "dolt_port", "dolt.auto-start: true"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("config should scrub %q:\n%s", forbidden, text)
		}
	}
	if !strings.Contains(text, "issue_prefix: gc") || !strings.Contains(text, "issue-prefix: gc") {
		t.Fatalf("config should normalize prefix keys:\n%s", text)
	}
}

func TestEnsureCanonicalConfigCollapsesDuplicateManagedKeys(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	input := strings.Join([]string{
		"issue_prefix: old",
		"issue_prefix: stale",
		"issue-prefix: old",
		"issue-prefix: stale",
		"gc.endpoint_origin: explicit",
		"gc.endpoint_origin: managed_city",
		"gc.endpoint_status: unverified",
		"gc.endpoint_status: verified",
		"dolt.auto-start: true",
		"dolt.auto-start: true",
		"",
	}, "\n")
	if err := fs.WriteFile(path, []byte(input), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := EnsureCanonicalConfig(fs, path, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	})
	if err != nil {
		t.Fatalf("EnsureCanonicalConfig() error = %v", err)
	}
	if !changed {
		t.Fatal("EnsureCanonicalConfig() should report duplicate cleanup changes")
	}

	data, err := fs.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, needle := range []string{
		"issue_prefix: gc",
		"issue-prefix: gc",
		"gc.endpoint_origin: managed_city",
		"gc.endpoint_status: verified",
		"dolt.auto-start: false",
	} {
		if count := countLineOccurrences(text, needle); count != 1 {
			t.Fatalf("config should contain exactly one %q, found %d:%c%s", needle, count, 10, text)
		}
	}
	for _, forbidden := range []string{
		"issue_prefix: stale",
		"issue-prefix: stale",
		"gc.endpoint_origin: explicit",
		"gc.endpoint_status: unverified",
		"dolt.auto-start: true",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("config should scrub stale duplicate %q:%c%s", forbidden, 10, text)
		}
	}
}

func TestEnsureCanonicalConfigForcesAutoExportOff(t *testing.T) {
	// bd's export.auto defaults to true and triggers a full-file import-then-export
	// cycle on every write. Managed cities never consume issues.jsonl (Dolt is the
	// source of truth), so this must be forced off at config time — not just via
	// BD_EXPORT_AUTO env-var suppression, which leaks when bd is invoked outside
	// the gc wrapper (agents, humans, bd setup).
	t.Run("sets false when key is absent", func(t *testing.T) {
		fs := fsys.OSFS{}
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		input := strings.Join([]string{
			"issue-prefix: gc",
			"dolt.auto-start: false",
			"",
		}, "\n")
		if err := fs.WriteFile(path, []byte(input), 0o644); err != nil {
			t.Fatal(err)
		}

		if _, err := EnsureCanonicalConfig(fs, path, ConfigState{
			IssuePrefix:    "gc",
			EndpointOrigin: EndpointOriginManagedCity,
			EndpointStatus: EndpointStatusVerified,
		}); err != nil {
			t.Fatalf("EnsureCanonicalConfig() error = %v", err)
		}

		data, err := fs.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "export.auto: false") {
			t.Fatalf("config should force export.auto: false:\n%s", data)
		}
	})

	t.Run("overrides explicit true", func(t *testing.T) {
		fs := fsys.OSFS{}
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		input := strings.Join([]string{
			"issue-prefix: gc",
			"export.auto: true",
			"",
		}, "\n")
		if err := fs.WriteFile(path, []byte(input), 0o644); err != nil {
			t.Fatal(err)
		}

		if _, err := EnsureCanonicalConfig(fs, path, ConfigState{
			IssuePrefix:    "gc",
			EndpointOrigin: EndpointOriginManagedCity,
			EndpointStatus: EndpointStatusVerified,
		}); err != nil {
			t.Fatalf("EnsureCanonicalConfig() error = %v", err)
		}

		data, err := fs.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if strings.Contains(text, "export.auto: true") {
			t.Fatalf("config should scrub export.auto: true:\n%s", text)
		}
		if !strings.Contains(text, "export.auto: false") {
			t.Fatalf("config should force export.auto: false:\n%s", text)
		}
	})
}

func TestEnsureCanonicalConfigWritesExternalFields(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	changed, err := EnsureCanonicalConfig(fs, path, ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: EndpointOriginExplicit,
		EndpointStatus: EndpointStatusUnverified,
		DoltHost:       "db.example.com",
		DoltPort:       "3307",
		DoltUser:       "agent",
	})
	if err != nil {
		t.Fatalf("EnsureCanonicalConfig() error = %v", err)
	}
	if !changed {
		t.Fatal("EnsureCanonicalConfig() should report changes")
	}

	data, err := fs.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, needle := range []string{
		"gc.endpoint_origin: explicit",
		"gc.endpoint_status: unverified",
		"dolt.host: db.example.com",
		"dolt.port: 3307",
		"dolt.user: agent",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("config missing %q:\n%s", needle, text)
		}
	}
}

func TestEnsureCanonicalConfigIsIdempotent(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	state := ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	}

	changed, err := EnsureCanonicalConfig(fs, path, state)
	if err != nil {
		t.Fatalf("first EnsureCanonicalConfig() error = %v", err)
	}
	if !changed {
		t.Fatal("first EnsureCanonicalConfig() should report changes")
	}

	changed, err = EnsureCanonicalConfig(fs, path, state)
	if err != nil {
		t.Fatalf("second EnsureCanonicalConfig() error = %v", err)
	}
	if changed {
		t.Fatal("second EnsureCanonicalConfig() should be idempotent")
	}
}

func TestEnsureCanonicalConfigFallsBackToLineRewriteOnMalformedYAML(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	input := strings.Join([]string{
		"issue-prefix: stale",
		"dolt.auto-start: true",
		"dolt_server_port: 3307",
		"dolt.password: should-not-stay",
		": not yaml",
		"",
	}, "\n")
	if err := fs.WriteFile(path, []byte(input), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := EnsureCanonicalConfig(fs, path, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	})
	if err != nil {
		t.Fatalf("EnsureCanonicalConfig() error = %v", err)
	}
	if !changed {
		t.Fatal("EnsureCanonicalConfig() should report changes for malformed YAML")
	}

	data, err := fs.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, needle := range []string{
		"issue_prefix: gc",
		"issue-prefix: gc",
		"dolt.auto-start: false",
		"gc.endpoint_origin: managed_city",
		"gc.endpoint_status: verified",
		": not yaml",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("config missing %q after malformed fallback:\n%s", needle, text)
		}
	}
	if strings.Contains(text, "dolt_server_port") {
		t.Fatalf("config should scrub deprecated port key after malformed fallback:\n%s", text)
	}
}

func TestEnsureCanonicalConfigFallbackIgnoresNestedManagedKeys(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	input := strings.Join([]string{
		"extra:",
		"  dolt.host: preserve-me",
		"dolt.host: stale.example.com",
		": not yaml",
		"",
	}, string(rune(10)))
	if err := fs.WriteFile(path, []byte(input), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := EnsureCanonicalConfig(fs, path, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginExplicit,
		EndpointStatus: EndpointStatusUnverified,
		DoltHost:       "db.example.com",
		DoltPort:       "4406",
	})
	if err != nil {
		t.Fatalf("EnsureCanonicalConfig() error = %v", err)
	}
	if !changed {
		t.Fatal("EnsureCanonicalConfig() should report changes for malformed YAML")
	}

	data, err := fs.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "  dolt.host: preserve-me") {
		t.Fatalf("fallback should preserve nested child content:%c%s", 10, text)
	}
	needle := string(rune(10)) + "dolt.host: db.example.com" + string(rune(10))
	if !strings.Contains(text, needle) {
		t.Fatalf("fallback should normalize the top-level host:%c%s", 10, text)
	}
}

func TestReadIssuePrefixPrefersCanonicalKey(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := fs.WriteFile(path, []byte("issue_prefix: gc\nissue-prefix: old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok, err := ReadIssuePrefix(fs, path)
	if err != nil {
		t.Fatalf("ReadIssuePrefix() error = %v", err)
	}
	if !ok || got != "gc" {
		t.Fatalf("ReadIssuePrefix() = (%q, %v), want (%q, true)", got, ok, "gc")
	}
}

func TestReadIssuePrefixFallsBackToLineScanOnMalformedYAML(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := fs.WriteFile(path, []byte("issue_prefix: gc\n: not yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok, err := ReadIssuePrefix(fs, path)
	if err != nil {
		t.Fatalf("ReadIssuePrefix() error = %v", err)
	}
	if !ok || got != "gc" {
		t.Fatalf("ReadIssuePrefix() = (%q, %v), want (%q, true)", got, ok, "gc")
	}
}

func TestReadIssuePrefixLineScanIgnoresNestedKeysOnMalformedYAML(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := fs.WriteFile(path, []byte(`extra:
  issue_prefix: nested
: not yaml
`), 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok, err := ReadIssuePrefix(fs, path)
	if err == nil {
		t.Fatal("ReadIssuePrefix() should surface malformed config when no top-level prefix exists")
	}
	if ok {
		t.Fatalf("ReadIssuePrefix() = (%q, %v), want no top-level prefix", got, ok)
	}
}

func TestReadAutoStartDisabledLineScanIgnoresNestedKeysOnMalformedYAML(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := fs.WriteFile(path, []byte(`extra:
  dolt.auto-start: false
: not yaml
`), 0o644); err != nil {
		t.Fatal(err)
	}

	disabled, err := ReadAutoStartDisabled(fs, path)
	if err == nil {
		t.Fatal("ReadAutoStartDisabled() should surface malformed config when no top-level flag exists")
	}
	if disabled {
		t.Fatal("ReadAutoStartDisabled() should ignore nested malformed fallback keys")
	}
}

func TestReadAutoStartDisabled(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := fs.WriteFile(path, []byte("dolt.auto-start: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	disabled, err := ReadAutoStartDisabled(fs, path)
	if err != nil {
		t.Fatalf("ReadAutoStartDisabled() error = %v", err)
	}
	if !disabled {
		t.Fatal("ReadAutoStartDisabled() = false, want true")
	}
}

func TestReadAutoStartDisabledFallsBackToLineScanOnMalformedYAML(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := fs.WriteFile(path, []byte("dolt.auto-start: false\n: not yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	disabled, err := ReadAutoStartDisabled(fs, path)
	if err != nil {
		t.Fatalf("ReadAutoStartDisabled() error = %v", err)
	}
	if !disabled {
		t.Fatal("ReadAutoStartDisabled() = false, want true")
	}
}

func TestEnsureCanonicalMetadataPreservesUnknownKeysAndScrubsDeprecatedOnes(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "metadata.json")
	input := `{"backend":"legacy","database":"old","dolt_database":"legacydb","custom":"keep","dolt_host":"127.0.0.1","dolt_user":"legacy","dolt_password":"secret","dolt_server_host":"legacy.example.com","dolt_server_port":"3307","dolt_server_user":"legacy-user","dolt_port":"4406"}`
	if err := fs.WriteFile(path, []byte(input), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := EnsureCanonicalMetadata(fs, path, MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: "hq",
	})
	if err != nil {
		t.Fatalf("EnsureCanonicalMetadata() error = %v", err)
	}
	if !changed {
		t.Fatal("EnsureCanonicalMetadata() should report changes")
	}

	data, err := fs.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if got := trimmedString(meta["custom"]); got != "keep" {
		t.Fatalf("custom = %q, want %q", got, "keep")
	}
	for _, key := range []string{"dolt_host", "dolt_user", "dolt_password", "dolt_server_host", "dolt_server_port", "dolt_server_user", "dolt_port"} {
		if _, ok := meta[key]; ok {
			t.Fatalf("metadata should not contain %q: %s", key, data)
		}
	}
	if got := trimmedString(meta["dolt_database"]); got != "hq" {
		t.Fatalf("dolt_database = %q, want %q", got, "hq")
	}
}

func TestEnsureCanonicalMetadataPreservesExistingDoltDatabaseWhenStateOmitsIt(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "metadata.json")
	input := `{"backend":"dolt","database":"dolt","dolt_mode":"server","dolt_database":"legacydb","custom":"keep"}`
	if err := fs.WriteFile(path, []byte(input), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := EnsureCanonicalMetadata(fs, path, MetadataState{
		Database: "dolt",
		Backend:  "dolt",
		DoltMode: "server",
	})
	if err != nil {
		t.Fatalf("EnsureCanonicalMetadata() error = %v", err)
	}
	if changed {
		t.Fatal("EnsureCanonicalMetadata() should preserve existing dolt_database when state omits it")
	}

	data, err := fs.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if got := trimmedString(meta["dolt_database"]); got != "legacydb" {
		t.Fatalf("dolt_database = %q, want %q", got, "legacydb")
	}
	if got := trimmedString(meta["custom"]); got != "keep" {
		t.Fatalf("custom = %q, want %q", got, "keep")
	}
}

func TestReadDoltDatabase(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "metadata.json")
	if err := fs.WriteFile(path, []byte(`{"dolt_database":"fe"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok, err := ReadDoltDatabase(fs, path)
	if err != nil {
		t.Fatalf("ReadDoltDatabase() error = %v", err)
	}
	if !ok || got != "fe" {
		t.Fatalf("ReadDoltDatabase() = (%q, %v), want (%q, true)", got, ok, "fe")
	}
}

func countLineOccurrences(text, needle string) int {
	count := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == needle {
			count++
		}
	}
	return count
}
