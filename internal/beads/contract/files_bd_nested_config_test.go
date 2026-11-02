package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
	"gopkg.in/yaml.v3"
)

// The testdata/bdconfig fixtures are real bd output, with the ephemeral Dolt
// test-server port replaced by 3310. Each was produced in an isolated HOME
// against a throwaway `dolt sql-server` (never ~/.beads or a live city):
//
//   - *-set-over-gc-canonical.yaml: the explicit-endpoint config.yaml gc's
//     EnsureCanonicalConfig writes (flat dolt.host/dolt.port plus the nested
//     dolt.disable-event-flush block), then `bd config set dolt.port 3310`,
//     `dolt.host 127.0.0.1`, `types.custom molecule,convoy` and
//     `dolt.user root`.
//   - *-set-over-init-template.yaml: the comment-only config.yaml `bd init
//     --server --external` writes, then `bd config set dolt.port 3310`,
//     `dolt.host 127.0.0.1`, `dolt.auto-start false` and `dolt.mode server`.
//
// bd 1.3.0 is the v1.3.0 tag; bd 1.3.1 is the hotfix/1.3.1 build (1.3.1-rc.1,
// f5a940279). 1.3.1 migrates a flat key it writes into the nested spelling
// (beads #6578), so its fixtures carry dolt.host/dolt.port only nested; 1.3.0
// updates a flat key in place but nests a key that has no flat line yet.
var bdConfigFixtures = []struct {
	file          string
	wantHost      string
	wantPort      string
	wantUser      string
	wantMode      string
	wantAutoStart bool // true when the fixture carries dolt.auto-start: false
}{
	{file: "bd-1.3.0-set-over-gc-canonical.yaml", wantHost: "127.0.0.1", wantPort: "3310", wantUser: "root", wantAutoStart: true},
	{file: "bd-1.3.1-set-over-gc-canonical.yaml", wantHost: "127.0.0.1", wantPort: "3310", wantUser: "root", wantAutoStart: true},
	{file: "bd-1.3.0-set-over-init-template.yaml", wantHost: "127.0.0.1", wantPort: "3310", wantMode: "server", wantAutoStart: true},
	{file: "bd-1.3.1-set-over-init-template.yaml", wantHost: "127.0.0.1", wantPort: "3310", wantMode: "server", wantAutoStart: true},
}

func copyBdConfigFixture(t *testing.T, name string, suffix string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "bdconfig", name))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, append(data, []byte(suffix)...), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadConfigStateReadsBdWrittenConfig(t *testing.T) {
	for _, tc := range bdConfigFixtures {
		for _, variant := range []struct {
			name   string
			suffix string
		}{
			{name: "parsed"},
			// A line YAML cannot parse routes every reader through the
			// line-scanning fallback, which must find the same values.
			{name: "line-scan", suffix: ": not yaml\n"},
		} {
			t.Run(tc.file+"/"+variant.name, func(t *testing.T) {
				fs := fsys.OSFS{}
				path := copyBdConfigFixture(t, tc.file, variant.suffix)

				got, ok, err := ReadConfigState(fs, path)
				if err != nil || !ok {
					t.Fatalf("ReadConfigState() = (%+v, %v, %v)", got, ok, err)
				}
				if got.DoltHost != tc.wantHost || got.DoltPort != tc.wantPort || got.DoltUser != tc.wantUser || got.DoltMode != tc.wantMode {
					t.Fatalf("ReadConfigState() host/port/user/mode = %q/%q/%q/%q, want %q/%q/%q/%q",
						got.DoltHost, got.DoltPort, got.DoltUser, got.DoltMode,
						tc.wantHost, tc.wantPort, tc.wantUser, tc.wantMode)
				}
				disabled, err := ReadAutoStartDisabled(fs, path)
				if err != nil {
					t.Fatalf("ReadAutoStartDisabled() error = %v", err)
				}
				if disabled != tc.wantAutoStart {
					t.Fatalf("ReadAutoStartDisabled() = %v, want %v", disabled, tc.wantAutoStart)
				}
			})
		}
	}
}

func TestFindConfigValuePrefersFlatSpelling(t *testing.T) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("dolt:\n  host: nested.example\ndolt.host: flat.example\n"), &doc); err != nil {
		t.Fatal(err)
	}
	node := findConfigValue(mappingRoot(&doc), "dolt.host")
	if node == nil || node.Value != "flat.example" {
		t.Fatalf("findConfigValue() = %v, want flat.example (bd resolves the flat key first)", node)
	}
	if got := findConfigValue(mappingRoot(&doc), "dolt.port"); got != nil {
		t.Fatalf("findConfigValue(dolt.port) = %v, want nil", got)
	}
}

// configKeySpellings counts how many times a dotted key appears in the file,
// flat or nested, so a test can assert gc left exactly one answer.
func configKeySpellings(t *testing.T, path, key string) int {
	t.Helper()
	doc, err := readConfigDoc(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	root := mappingRoot(doc)
	count := 0
	if findValue(root, key) != nil {
		count++
	}
	section, field, _ := strings.Cut(key, ".")
	if sec := findValue(root, section); sec != nil && findValue(sec, field) != nil {
		count++
	}
	return count
}

func TestEnsureCanonicalConfigConvergesBdNestedEndpoint(t *testing.T) {
	for _, tc := range bdConfigFixtures {
		t.Run(tc.file, func(t *testing.T) {
			fs := fsys.OSFS{}
			path := copyBdConfigFixture(t, tc.file, "")

			state := ConfigState{
				IssuePrefix:    "ex",
				EndpointOrigin: EndpointOriginExplicit,
				EndpointStatus: EndpointStatusVerified,
				DoltHost:       "db.example.com",
				DoltPort:       "4406",
			}
			if _, err := EnsureCanonicalConfig(fs, path, state); err != nil {
				t.Fatalf("EnsureCanonicalConfig() error = %v", err)
			}
			got, _, err := ReadConfigState(fs, path)
			if err != nil {
				t.Fatal(err)
			}
			if got.DoltHost != "db.example.com" || got.DoltPort != "4406" || got.DoltUser != "" || got.DoltMode != "" {
				t.Fatalf("after canonicalize host/port/user/mode = %q/%q/%q/%q", got.DoltHost, got.DoltPort, got.DoltUser, got.DoltMode)
			}
			for key, want := range map[string]int{
				"dolt.host":       1,
				"dolt.port":       1,
				"dolt.user":       0,
				"dolt.mode":       0,
				"dolt.auto-start": 1,
			} {
				if n := configKeySpellings(t, path, key); n != want {
					data, _ := os.ReadFile(path)
					t.Fatalf("%s spelled %d times, want %d:\n%s", key, n, want, data)
				}
			}
			dolt, ok, err := ReadDoltConfig(fs, path)
			if err != nil || !ok || dolt.DisableEventFlush == nil || !*dolt.DisableEventFlush {
				t.Fatalf("ReadDoltConfig() = (%+v, %v, %v), want disable-event-flush kept", dolt, ok, err)
			}

			changed, err := EnsureCanonicalConfig(fs, path, state)
			if err != nil {
				t.Fatalf("second EnsureCanonicalConfig() error = %v", err)
			}
			if changed {
				data, _ := os.ReadFile(path)
				t.Fatalf("second EnsureCanonicalConfig() changed = true, want idempotent:\n%s", data)
			}
		})
	}
}

func TestEnsureCanonicalConfigDropsBdNestedEndpointTheStateDoesNotOwn(t *testing.T) {
	fs := fsys.OSFS{}
	path := copyBdConfigFixture(t, "bd-1.3.1-set-over-gc-canonical.yaml", "")

	// A managed-city scope must not track an endpoint. Before, gc deleted only
	// the flat spelling, so the nested one bd 1.3.1 wrote survived and bd kept
	// dialing it.
	if _, err := EnsureCanonicalConfig(fs, path, ConfigState{
		IssuePrefix:    "ex",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	}); err != nil {
		t.Fatalf("EnsureCanonicalConfig() error = %v", err)
	}
	for _, key := range []string{"dolt.host", "dolt.port", "dolt.user"} {
		if n := configKeySpellings(t, path, key); n != 0 {
			data, _ := os.ReadFile(path)
			t.Fatalf("%s still present (%d spellings):\n%s", key, n, data)
		}
	}
	dolt, ok, err := ReadDoltConfig(fs, path)
	if err != nil || !ok || dolt.DisableEventFlush == nil || !*dolt.DisableEventFlush {
		t.Fatalf("ReadDoltConfig() = (%+v, %v, %v), want the dolt block's gc key kept", dolt, ok, err)
	}
}

func TestEnsureCanonicalConfigFallbackDropsBdNestedEndpoint(t *testing.T) {
	fs := fsys.OSFS{}
	path := copyBdConfigFixture(t, "bd-1.3.1-set-over-init-template.yaml", ": not yaml\n")

	if _, err := EnsureCanonicalConfig(fs, path, ConfigState{
		IssuePrefix:    "ex",
		EndpointOrigin: EndpointOriginExplicit,
		EndpointStatus: EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       "4406",
	}); err != nil {
		t.Fatalf("EnsureCanonicalConfig() error = %v", err)
	}
	data, err := fs.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, gone := range []string{"    host: 127.0.0.1", "    port: 3310", "    mode: server", "    auto-start: false"} {
		if strings.Contains(text, gone+"\n") {
			t.Fatalf("fallback kept nested %q:\n%s", gone, text)
		}
	}
	got := readConfigStateFromData(data)
	if got.DoltHost != "db.example.com" || got.DoltPort != "4406" || got.DoltMode != "" {
		t.Fatalf("after fallback host/port/mode = %q/%q/%q", got.DoltHost, got.DoltPort, got.DoltMode)
	}
	if !strings.Contains(text, "dolt:\n  disable-event-flush: true\n") {
		t.Fatalf("fallback should leave one dolt block holding gc's nested key:\n%s", text)
	}
}

func TestScanConfigLineValuePrefersDirectNestedChild(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "direct child after deeper mapping",
			input: "dolt:\n  sub:\n    host: deeper.example\n  host: direct.example\n: not yaml\n",
			want:  "direct.example",
		},
		{
			name:  "only deeper mapping",
			input: "dolt:\n  sub:\n    host: deeper.example\n: not yaml\n",
			want:  "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := readConfigStateFromData([]byte(tc.input)).DoltHost; got != tc.want {
				t.Fatalf("DoltHost = %q, want %q", got, tc.want)
			}
		})
	}
}
