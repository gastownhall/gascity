package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
	"gopkg.in/yaml.v3"
)

// bdInitTemplate is the shape of the config.yaml bd init writes: comments only.
const bdInitTemplate = "# Beads Configuration File\n# This file configures default behavior for all bd commands in this repository.\n\n# issue-prefix: \"\"\n"

func writeSharedServerFixture(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// decodeSharedServer reads the pinned value the way bd's viper would see the
// nested key, so the tests check the file's meaning rather than its bytes.
func decodeSharedServer(t *testing.T, path string) (any, map[string]any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("pinned config does not parse: %v\n%s", err, data)
	}
	dolt, _ := doc["dolt"].(map[string]any)
	return dolt["shared-server"], doc
}

func TestEnsureSharedServerDisabledPinsBdInitTemplateAndKeepsItsComments(t *testing.T) {
	path := writeSharedServerFixture(t, bdInitTemplate)

	changed, previous, err := EnsureSharedServerDisabled(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || previous != SharedServerUnset {
		t.Fatalf("changed=%v previous=%v, want true/unset", changed, previous)
	}
	got, _ := decodeSharedServer(t, path)
	if got != false {
		t.Fatalf("dolt.shared-server = %#v, want false", got)
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), bdInitTemplate) {
		t.Fatalf("bd's template comments were not preserved:\n%s", data)
	}
	if pin, err := ReadSharedServerPin(fsys.OSFS{}, path); err != nil || pin != SharedServerPinnedOff {
		t.Fatalf("ReadSharedServerPin = %v, %v; want pinned off", pin, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config mode = %o, want the 0600 bd wrote", perm)
	}

	changed, previous, err = EnsureSharedServerDisabled(fsys.OSFS{}, path)
	if err != nil || changed || previous != SharedServerPinnedOff {
		t.Fatalf("second pass changed=%v previous=%v err=%v; want an idempotent no-op", changed, previous, err)
	}
}

// A scope bd init bound to the shared server carries the key itself (bd
// persists it as a flat dotted key). The pin replaces it with one nested
// answer and reports what it replaced, and leaves every other key alone.
func TestEnsureSharedServerDisabledReplacesAnExistingOnPin(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"flat", "issue_prefix: hq\ndolt.shared-server: true\ndolt:\n  disable-event-flush: true\n"},
		{"nested", "issue_prefix: hq\ndolt:\n  disable-event-flush: true\n  shared-server: true\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeSharedServerFixture(t, tc.body)
			if pin, err := ReadSharedServerPin(fsys.OSFS{}, path); err != nil || pin != SharedServerPinnedOn {
				t.Fatalf("ReadSharedServerPin before = %v, %v; want pinned on", pin, err)
			}
			changed, previous, err := EnsureSharedServerDisabled(fsys.OSFS{}, path)
			if err != nil {
				t.Fatal(err)
			}
			if !changed || previous != SharedServerPinnedOn {
				t.Fatalf("changed=%v previous=%v, want true/pinned-on", changed, previous)
			}
			got, doc := decodeSharedServer(t, path)
			if got != false {
				t.Fatalf("dolt.shared-server = %#v, want false", got)
			}
			if _, flat := doc[SharedServerConfigKey]; flat {
				t.Fatalf("flat %s key survived: %v", SharedServerConfigKey, doc)
			}
			if doc["issue_prefix"] != "hq" {
				t.Fatalf("issue_prefix = %#v, want hq preserved", doc["issue_prefix"])
			}
			if dolt, _ := doc["dolt"].(map[string]any); dolt["disable-event-flush"] != true {
				t.Fatalf("dolt.disable-event-flush lost: %v", doc["dolt"])
			}
		})
	}
}

func TestEnsureSharedServerDisabledCreatesAMissingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if pin, err := ReadSharedServerPin(fsys.OSFS{}, path); err != nil || pin != SharedServerUnset {
		t.Fatalf("ReadSharedServerPin(missing) = %v, %v; want unset", pin, err)
	}
	changed, _, err := EnsureSharedServerDisabled(fsys.OSFS{}, path)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if got, _ := decodeSharedServer(t, path); got != false {
		t.Fatalf("dolt.shared-server = %#v, want false", got)
	}
}

// EnsureCanonicalConfig is the other writer of a scope's config.yaml; it must
// not drop the pin when it canonicalizes the file afterwards.
func TestEnsureCanonicalConfigPreservesTheSharedServerPin(t *testing.T) {
	path := writeSharedServerFixture(t, bdInitTemplate)
	if _, _, err := EnsureSharedServerDisabled(fsys.OSFS{}, path); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureCanonicalConfig(fsys.OSFS{}, path, ConfigState{IssuePrefix: "hq", EndpointOrigin: EndpointOriginManagedCity}); err != nil {
		t.Fatal(err)
	}
	if pin, err := ReadSharedServerPin(fsys.OSFS{}, path); err != nil || pin != SharedServerPinnedOff {
		data, _ := os.ReadFile(path)
		t.Fatalf("pin after canonicalization = %v, %v; want pinned off\n%s", pin, err, data)
	}
}
