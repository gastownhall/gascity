package exec //nolint:revive // internal package, always imported with alias

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// writeImportScript writes an exec provider script that answers capabilities
// with caps (or does not know the op when caps is "unknown"), and stores the
// bead an import op reads in dir/imported.json, echoing it back.
func writeImportScript(t *testing.T, caps string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	capabilities := "  capabilities) printf '%s\\n' '" + caps + "' ;;\n"
	if caps == "unknown" {
		capabilities = ""
	}
	script := "#!/bin/sh\ncase \"$1\" in\n" + capabilities +
		"  import) cat > \"" + filepath.Join(dir, "imported.json") + "\"; cat \"" + filepath.Join(dir, "imported.json") + "\" ;;\n" +
		"  *) exit 2 ;;\nesac\n"
	path := filepath.Join(dir, "provider.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, filepath.Join(dir, "imported.json")
}

// TestExecImportKeepsTheWholeBead: a provider that lists import among its
// capabilities takes a bead under its own id with its status, timestamps and
// fields, and a provider that does not is refused before it is asked to write.
func TestExecImportKeepsTheWholeBead(t *testing.T) {
	for _, caps := range []string{`["import","snapshot"]`, "snapshot import"} {
		t.Run(caps, func(t *testing.T) {
			script, imported := writeImportScript(t, caps)
			store := NewStore(script)
			created := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
			got, err := store.CreateWithForeignID(beads.Bead{ID: "gcg-12", Title: "a step", Status: "closed", Type: "task", CreatedAt: created, UpdatedAt: created.Add(time.Hour), CloseReason: "done", Labels: []string{"l"}, Metadata: beads.StringMap{"k": "v"}, Ephemeral: true})
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != "gcg-12" || got.Status != "closed" || !got.CreatedAt.Equal(created) || got.CloseReason != "done" || got.Metadata["k"] != "v" || !got.Ephemeral {
				t.Fatalf("the provider returned %+v", got)
			}
			var wire map[string]any
			data, _ := os.ReadFile(imported)
			if err := json.Unmarshal(data, &wire); err != nil {
				t.Fatal(err)
			}
			if wire["id"] != "gcg-12" || wire["status"] != "closed" || wire["updated_at"] != "2026-08-01T13:00:00Z" {
				t.Fatalf("the import op read %s", data)
			}
		})
	}
	for _, caps := range []string{"unknown", "snapshot", ""} {
		t.Run("without import: "+caps, func(t *testing.T) {
			script, imported := writeImportScript(t, caps)
			store := NewStore(script)
			if ok, err := store.ImportSupported(); ok || err != nil {
				t.Fatalf("ImportSupported = %v, %v", ok, err)
			}
			if _, err := store.CreateWithForeignID(beads.Bead{ID: "gcg-12", Title: "x"}); !errors.Is(err, ErrImportUnsupported) {
				t.Fatalf("CreateWithForeignID = %v, want ErrImportUnsupported", err)
			}
			if _, err := os.Stat(imported); err == nil {
				t.Fatal("the store asked a provider without the op to import")
			}
		})
	}
	if _, err := NewStore("/bin/true").CreateWithForeignID(beads.Bead{}); err == nil || !strings.Contains(err.Error(), "empty id") {
		t.Fatalf("an id-less import = %v", err)
	}
}
