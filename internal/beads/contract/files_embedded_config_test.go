package contract

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func writeEmbeddedConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// cleanEmbeddedBdConfig is the shape bd itself leaves in an embedded repo: its
// commented template plus bd-owned keys, and no issue prefix or custom types —
// bd keeps both in the store (config table / custom_types table).
const cleanEmbeddedBdConfig = `# Beads Configuration File
# This file configures default behavior for all bd commands in this repository

# Use no-db mode: JSONL-only, no Dolt database
# no-db: false

sync.branch: main
dolt.mode: embedded
`

// assertScrubRemoved checks the claims a scrub reports against the ones it was
// expected to take out, in order: the report is what gc logs about a file it
// does not own, so it must name exactly what changed.
func assertScrubRemoved(t *testing.T, removed []string, err error, want []string) {
	t.Helper()
	if err != nil || !slices.Equal(removed, want) {
		t.Fatalf("ScrubEmbeddedScopeConfig = (%q, %v), want (%q, nil)", removed, err, want)
	}
}

// A clean embedded config is bd's, not gc's: the scrub leaves every byte in
// place and reports no change.
func TestScrubEmbeddedScopeConfigLeavesACleanFileByteIdentical(t *testing.T) {
	path := writeEmbeddedConfig(t, cleanEmbeddedBdConfig)
	removed, err := ScrubEmbeddedScopeConfig(fsys.OSFS{}, path)
	assertScrubRemoved(t, removed, err, nil)
	if got := readConfigFile(t, path); got != cleanEmbeddedBdConfig {
		t.Fatalf("clean embedded config was rewritten:\n%s", got)
	}
}

// The scrub never materializes a config.yaml bd did not write.
func TestScrubEmbeddedScopeConfigDoesNotCreateAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	removed, err := ScrubEmbeddedScopeConfig(fsys.OSFS{}, path)
	assertScrubRemoved(t, removed, err, nil)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("missing config.yaml was created (stat err = %v)", err)
	}
}

// The endpoint mirror is gc's only when gc's marker says so; policy keys, the
// vocabulary keys and a dolt.mode agreeing with metadata are never gc's to
// remove.
func TestScrubEmbeddedScopeConfigRemovesOnlyGcOwnedKeys(t *testing.T) {
	for name, tc := range map[string]struct {
		in       string
		gone     []string
		retained []string
	}{
		"gc-stamped endpoint block": {
			in:       "issue_prefix: fr\ntypes.custom: molecule\ngc.endpoint_origin: inherited_city\ngc.endpoint_status: verified\ndolt.mode: server\ndolt.host: 127.0.0.1\ndolt.port: 3307\ndolt.socket: /tmp/dolt.sock\ndolt.user: root\nbackup.enabled: false\nexport.auto: false\ndolt.auto-start: false\n",
			gone:     []string{"gc.endpoint_origin", "gc.endpoint_status", "dolt.mode", "dolt.host", "dolt.port", "dolt.socket", "dolt.user"},
			retained: []string{"issue_prefix: fr", "types.custom: molecule", "backup.enabled: false", "export.auto: false", "dolt.auto-start: false"},
		},
		"no gc marker": {
			in:       "dolt.mode: embedded\ndolt.port: 3310\n",
			retained: []string{"dolt.mode: embedded", "dolt.port: 3310"},
		},
		"contradicting mode without marker": {
			in:       "dolt.mode: server\ndolt.host: 10.0.0.1\n",
			gone:     []string{"dolt.mode"},
			retained: []string{"dolt.host: 10.0.0.1"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := writeEmbeddedConfig(t, tc.in)
			removed, err := ScrubEmbeddedScopeConfig(fsys.OSFS{}, path)
			assertScrubRemoved(t, removed, err, tc.gone)
			got := readConfigFile(t, path)
			for _, key := range tc.gone {
				if strings.Contains(got, key+":") {
					t.Errorf("%s survived:\n%s", key, got)
				}
			}
			for _, want := range tc.retained {
				if !strings.Contains(got, want) {
					t.Errorf("%q was removed:\n%s", want, got)
				}
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Errorf("mode = %v, want bd's 0600 preserved", info.Mode().Perm())
			}
			removed, err = ScrubEmbeddedScopeConfig(fsys.OSFS{}, path)
			assertScrubRemoved(t, removed, err, nil)
		})
	}
}

// bd writes a `bd config set` key nested (`dolt:` / `  host:`) — 1.3.0 when
// the key has no flat line yet, 1.3.1 always — and the scrub reads either
// spelling, so it clears both: a nested endpoint left behind a deleted
// gc.endpoint_origin is a server claim no later pass can prove gc wrote, and
// a nested server mode keeps contradicting metadata.
func TestScrubEmbeddedScopeConfigClearsBdNestedSpellings(t *testing.T) {
	type keys struct{ gone, retained []string }
	gcStamped := keys{
		gone:     []string{"gc.endpoint_origin", "gc.endpoint_status", "dolt.host", "dolt.port", "dolt.user"},
		retained: []string{"dolt.disable-event-flush", "dolt.auto-start", "export.auto", "backup.enabled", "types.custom"},
	}
	unmarkedServerMode := keys{
		gone:     []string{"dolt.mode"},
		retained: []string{"dolt.host", "dolt.port", "dolt.auto-start"},
	}
	for fixture, want := range map[string]keys{
		"bd-1.3.0-set-over-gc-canonical.yaml":  gcStamped,
		"bd-1.3.1-set-over-gc-canonical.yaml":  gcStamped,
		"bd-1.3.0-set-over-init-template.yaml": unmarkedServerMode,
		"bd-1.3.1-set-over-init-template.yaml": unmarkedServerMode,
	} {
		t.Run(fixture, func(t *testing.T) {
			path := copyBdConfigFixture(t, fixture, "")
			removed, err := ScrubEmbeddedScopeConfig(fsys.OSFS{}, path)
			assertScrubRemoved(t, removed, err, want.gone)
			got := readConfigFile(t, path)
			for _, key := range want.gone {
				if n := configKeySpellings(t, path, key); n != 0 {
					t.Errorf("%s survived in %d spelling(s):\n%s", key, n, got)
				}
			}
			for _, key := range want.retained {
				if n := configKeySpellings(t, path, key); n != 1 {
					t.Errorf("%s spelled %d times, want it kept once:\n%s", key, n, got)
				}
			}
			removed, err = ScrubEmbeddedScopeConfig(fsys.OSFS{}, path)
			assertScrubRemoved(t, removed, err, nil)
		})
	}
}

// gc's marker is read in either spelling, so it is cleared in either: a nested
// gc.endpoint_* left behind would go on claiming the endpoint.
func TestScrubEmbeddedScopeConfigClearsANestedGcMarker(t *testing.T) {
	path := writeEmbeddedConfig(t, "gc:\n    endpoint_origin: inherited_city\n    endpoint_status: verified\ndolt:\n    host: 127.0.0.1\n    port: 3307\n")
	gone := []string{"gc.endpoint_origin", "gc.endpoint_status", "dolt.host", "dolt.port"}
	removed, err := ScrubEmbeddedScopeConfig(fsys.OSFS{}, path)
	assertScrubRemoved(t, removed, err, gone)
	for _, key := range gone {
		if n := configKeySpellings(t, path, key); n != 0 {
			t.Errorf("%s survived in %d spelling(s):\n%s", key, n, readConfigFile(t, path))
		}
	}
	removed, err = ScrubEmbeddedScopeConfig(fsys.OSFS{}, path)
	assertScrubRemoved(t, removed, err, nil)
}

// bd's file is not repaired into gc's shape when it does not parse.
func TestScrubEmbeddedScopeConfigLeavesUnparseableFileAlone(t *testing.T) {
	const malformed = "gc.endpoint_origin: inherited_city\n  bad: [indent\n"
	path := writeEmbeddedConfig(t, malformed)
	removed, err := ScrubEmbeddedScopeConfig(fsys.OSFS{}, path)
	assertScrubRemoved(t, removed, err, nil)
	if got := readConfigFile(t, path); got != malformed {
		t.Fatalf("unparseable config was rewritten:\n%s", got)
	}
}
