package contract

import (
	"os"
	"path/filepath"
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

// A clean embedded config is bd's, not gc's: the scrub leaves every byte in
// place and reports no change.
func TestScrubEmbeddedScopeConfigLeavesACleanFileByteIdentical(t *testing.T) {
	path := writeEmbeddedConfig(t, cleanEmbeddedBdConfig)
	changed, err := ScrubEmbeddedScopeConfig(fsys.OSFS{}, path)
	if err != nil || changed {
		t.Fatalf("ScrubEmbeddedScopeConfig = (%v, %v), want (false, nil)", changed, err)
	}
	if got := readConfigFile(t, path); got != cleanEmbeddedBdConfig {
		t.Fatalf("clean embedded config was rewritten:\n%s", got)
	}
}

// The scrub never materializes a config.yaml bd did not write.
func TestScrubEmbeddedScopeConfigDoesNotCreateAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	changed, err := ScrubEmbeddedScopeConfig(fsys.OSFS{}, path)
	if err != nil || changed {
		t.Fatalf("ScrubEmbeddedScopeConfig = (%v, %v), want (false, nil)", changed, err)
	}
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
			in:       "issue_prefix: fr\ntypes.custom: molecule\ngc.endpoint_origin: inherited_city\ngc.endpoint_status: verified\ndolt.mode: server\ndolt.host: 127.0.0.1\ndolt.port: 3307\ndolt.user: root\nbackup.enabled: false\nexport.auto: false\ndolt.auto-start: false\n",
			gone:     []string{"gc.endpoint_origin", "gc.endpoint_status", "dolt.mode", "dolt.host", "dolt.port", "dolt.user"},
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
			if _, err := ScrubEmbeddedScopeConfig(fsys.OSFS{}, path); err != nil {
				t.Fatal(err)
			}
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
			if changed, err := ScrubEmbeddedScopeConfig(fsys.OSFS{}, path); err != nil || changed {
				t.Fatalf("second pass = (%v, %v), want (false, nil)", changed, err)
			}
		})
	}
}

// bd's file is not repaired into gc's shape when it does not parse.
func TestScrubEmbeddedScopeConfigLeavesUnparseableFileAlone(t *testing.T) {
	const malformed = "gc.endpoint_origin: inherited_city\n  bad: [indent\n"
	path := writeEmbeddedConfig(t, malformed)
	changed, err := ScrubEmbeddedScopeConfig(fsys.OSFS{}, path)
	if err != nil || changed {
		t.Fatalf("ScrubEmbeddedScopeConfig = (%v, %v), want (false, nil)", changed, err)
	}
	if got := readConfigFile(t, path); got != malformed {
		t.Fatalf("unparseable config was rewritten:\n%s", got)
	}
}
