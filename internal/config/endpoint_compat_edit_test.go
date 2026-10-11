package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

const commentedCityToml = `# City header comment — operational notes live here.
# EMERGENCY ROLLBACK: cp city.toml.bak city.toml && gc kickstart

[workspace]
name = "demo" # workspace trailing comment

# Why the dolt endpoint is pinned: incident 2026-07-14.
[dolt]
# host rationale comment
host = "100.93.120.2" # tailnet IP, see sys-m8qt
port = 3307
archive_level = 1 # keep compaction on

[[rigs]]
name = "frontend" # rig trailing comment
prefix = "fe"
# dolt_host pinned for the laptop writer
dolt_host = "100.93.120.2"
dolt_port = "3307"

[[rigs]]
name = "ops"
prefix = "ops"
dolt_host = "ops-db.example.com" # explicit endpoint
dolt_port = "5501"
`

func writeEndpointEditFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "city.toml")
	if err := (fsys.OSFS{}).WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readEndpointEditFixture(t *testing.T, path string) string {
	t.Helper()
	data, err := (fsys.OSFS{}).ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestApplyCityEndpointKeyEditsReplacesValuesPreservingEverythingElse(t *testing.T) {
	path := writeEndpointEditFixture(t, commentedCityToml)
	ok, err := ApplyCityEndpointKeyEditsInPlace(fsys.OSFS{}, path, []CityEndpointKeyEdit{
		{Key: "host", Value: `"127.0.0.1"`},
		{Key: "port", Value: "4406"},
		{RigName: "frontend", Key: "dolt_host", Value: `"127.0.0.1"`},
		{RigName: "frontend", Key: "dolt_port", Value: `"4406"`},
	})
	if err != nil {
		t.Fatalf("ApplyCityEndpointKeyEditsInPlace: %v", err)
	}
	if !ok {
		t.Fatalf("ApplyCityEndpointKeyEditsInPlace ok = false, want in-place edit")
	}
	got := readEndpointEditFixture(t, path)
	want := strings.NewReplacer(
		`host = "100.93.120.2" # tailnet IP, see sys-m8qt`, `host = "127.0.0.1" # tailnet IP, see sys-m8qt`,
		"port = 3307\n", "port = 4406\n",
		"dolt_host = \"100.93.120.2\"\ndolt_port = \"3307\"", "dolt_host = \"127.0.0.1\"\ndolt_port = \"4406\"",
	).Replace(commentedCityToml)
	if got != want {
		t.Fatalf("edited city.toml = %q, want %q", got, want)
	}
}

func TestApplyCityEndpointKeyEditsInsertsMissingKeyIntoExistingTable(t *testing.T) {
	content := `# header
[dolt]
host = "db.example.com"

[[rigs]]
name = "frontend"
prefix = "fe"
`
	path := writeEndpointEditFixture(t, content)
	ok, err := ApplyCityEndpointKeyEditsInPlace(fsys.OSFS{}, path, []CityEndpointKeyEdit{
		{Key: "port", Value: "4406"},
		{RigName: "frontend", Key: "dolt_host", Value: `"db.example.com"`},
	})
	if err != nil || !ok {
		t.Fatalf("ApplyCityEndpointKeyEditsInPlace = (%v, %v), want (true, nil)", ok, err)
	}
	got := readEndpointEditFixture(t, path)
	if !strings.HasPrefix(got, "# header\n") {
		t.Fatalf("header comment lost: %q", got)
	}
	cfg := decodeEndpointEditConfig(t, got)
	if cfg.Dolt.Host != "db.example.com" || cfg.Dolt.Port != 4406 {
		t.Fatalf("dolt = %+v", cfg.Dolt)
	}
	if len(cfg.Rigs) != 1 || cfg.Rigs[0].DoltHost != "db.example.com" {
		t.Fatalf("rigs = %+v", cfg.Rigs)
	}
}

func TestApplyCityEndpointKeyEditsAppendsMissingDoltTable(t *testing.T) {
	content := `# keep me
[workspace]
name = "demo"
`
	path := writeEndpointEditFixture(t, content)
	ok, err := ApplyCityEndpointKeyEditsInPlace(fsys.OSFS{}, path, []CityEndpointKeyEdit{
		{Key: "host", Value: `"db.example.com"`},
		{Key: "port", Value: "4406"},
	})
	if err != nil || !ok {
		t.Fatalf("ApplyCityEndpointKeyEditsInPlace = (%v, %v), want (true, nil)", ok, err)
	}
	got := readEndpointEditFixture(t, path)
	if !strings.Contains(got, "# keep me") {
		t.Fatalf("comment lost: %q", got)
	}
	cfg := decodeEndpointEditConfig(t, got)
	if cfg.Dolt.Host != "db.example.com" || cfg.Dolt.Port != 4406 {
		t.Fatalf("dolt = %+v", cfg.Dolt)
	}
}

// TestApplyCityEndpointKeyEditsRoundTripsAppendedDoltTable is the byte-identical
// round trip: use-external appends a [dolt] table, use-managed removes its keys,
// and the file must come back exactly as it started. Before the emptied-section
// drop, removing the last key left a stray "[dolt]" header and its blank
// separator behind — comments survived, but the round trip did not.
func TestApplyCityEndpointKeyEditsRoundTripsAppendedDoltTable(t *testing.T) {
	original := `# keep me
[workspace]
name = "demo"
`
	path := writeEndpointEditFixture(t, original)

	ok, err := ApplyCityEndpointKeyEditsInPlace(fsys.OSFS{}, path, []CityEndpointKeyEdit{
		{Key: "host", Value: `"db.example.com"`},
		{Key: "port", Value: "4406"},
	})
	if err != nil || !ok {
		t.Fatalf("append edits = (%v, %v), want (true, nil)", ok, err)
	}

	ok, err = ApplyCityEndpointKeyEditsInPlace(fsys.OSFS{}, path, []CityEndpointKeyEdit{
		{Key: "host"},
		{Key: "port"},
	})
	if err != nil || !ok {
		t.Fatalf("removal edits = (%v, %v), want (true, nil)", ok, err)
	}

	if got := readEndpointEditFixture(t, path); got != original {
		t.Fatalf("round trip not byte-identical:\n got: %q\nwant: %q", got, original)
	}
}

// TestApplyCityEndpointKeyEditsKeepsPreexistingEmptyDoltTableOnRigOnlyEdit: a
// [dolt] table that was already bare before the edit is not this edit set's to
// clean up. A rig-only edit must leave the header — and the comment above it —
// exactly where the user put them.
func TestApplyCityEndpointKeyEditsKeepsPreexistingEmptyDoltTableOnRigOnlyEdit(t *testing.T) {
	original := `[workspace]
name = "demo"

# the endpoint lives in .beads/config.yaml; this table is intentionally bare
[dolt]

[[rigs]]
name = "frontend"
prefix = "fe"
dolt_host = "stale.example.com"
dolt_port = "3307"
`
	path := writeEndpointEditFixture(t, original)
	ok, err := ApplyCityEndpointKeyEditsInPlace(fsys.OSFS{}, path, []CityEndpointKeyEdit{
		{RigName: "frontend", Key: "dolt_host", Value: `"127.0.0.1"`},
		{RigName: "frontend", Key: "dolt_port", Value: `"4406"`},
	})
	if err != nil || !ok {
		t.Fatalf("ApplyCityEndpointKeyEditsInPlace = (%v, %v), want (true, nil)", ok, err)
	}
	got := readEndpointEditFixture(t, path)
	want := strings.NewReplacer(
		`dolt_host = "stale.example.com"`, `dolt_host = "127.0.0.1"`,
		`dolt_port = "3307"`, `dolt_port = "4406"`,
	).Replace(original)
	if got != want {
		t.Fatalf("rig-only edit disturbed the pre-existing [dolt] table:\n got: %q\nwant: %q", got, want)
	}
}

// TestApplyCityEndpointKeyEditsKeepsDoltHeaderHoldingAComment: an emptied [dolt]
// table whose body still carries a comment keeps its header. Dropping it would
// reparent the comment under the previous table — the opposite of this file's
// whole purpose.
func TestApplyCityEndpointKeyEditsKeepsDoltHeaderHoldingAComment(t *testing.T) {
	content := `[workspace]
name = "demo"

[dolt]
# why this endpoint was pinned: incident 2026-07-14
host = "db.example.com"
`
	path := writeEndpointEditFixture(t, content)
	ok, err := ApplyCityEndpointKeyEditsInPlace(fsys.OSFS{}, path, []CityEndpointKeyEdit{{Key: "host"}})
	if err != nil || !ok {
		t.Fatalf("ApplyCityEndpointKeyEditsInPlace = (%v, %v), want (true, nil)", ok, err)
	}
	got := readEndpointEditFixture(t, path)
	if !strings.Contains(got, "[dolt]") {
		t.Fatalf("[dolt] header dropped, orphaning its comment: %q", got)
	}
	if !strings.Contains(got, "# why this endpoint was pinned") {
		t.Fatalf("comment lost: %q", got)
	}
	if cfg := decodeEndpointEditConfig(t, got); cfg.Dolt.Host != "" {
		t.Fatalf("dolt = %+v, want host cleared", cfg.Dolt)
	}
}

func TestApplyCityEndpointKeyEditsRemovesKeysPreservingComments(t *testing.T) {
	path := writeEndpointEditFixture(t, commentedCityToml)
	ok, err := ApplyCityEndpointKeyEditsInPlace(fsys.OSFS{}, path, []CityEndpointKeyEdit{
		{Key: "host"},
		{Key: "port"},
		{RigName: "frontend", Key: "dolt_host"},
		{RigName: "frontend", Key: "dolt_port"},
	})
	if err != nil || !ok {
		t.Fatalf("ApplyCityEndpointKeyEditsInPlace = (%v, %v), want (true, nil)", ok, err)
	}
	got := readEndpointEditFixture(t, path)
	cfg := decodeEndpointEditConfig(t, got)
	if cfg.Dolt.Host != "" || cfg.Dolt.Port != 0 {
		t.Fatalf("dolt = %+v, want host/port cleared", cfg.Dolt)
	}
	if cfg.Dolt.ArchiveLevel == nil || *cfg.Dolt.ArchiveLevel != 1 {
		t.Fatalf("archive_level = %v, want preserved 1", cfg.Dolt.ArchiveLevel)
	}
	if cfg.Rigs[0].DoltHost != "" || cfg.Rigs[0].DoltPort != "" {
		t.Fatalf("frontend rig = %+v, want dolt keys removed", cfg.Rigs[0])
	}
	if cfg.Rigs[1].DoltHost != "ops-db.example.com" {
		t.Fatalf("ops rig = %+v, want untouched", cfg.Rigs[1])
	}
	for _, comment := range []string{
		"# City header comment", "# EMERGENCY ROLLBACK", "# Why the dolt endpoint is pinned",
		"# host rationale comment", "# dolt_host pinned for the laptop writer", "# rig trailing comment",
	} {
		if !strings.Contains(got, comment) {
			t.Fatalf("comment %q lost: %q", comment, got)
		}
	}
}

func TestApplyCityEndpointKeyEditsRefusesInlineTableLayout(t *testing.T) {
	content := `# comment
dolt = { host = "db.example.com", port = 3307 }
`
	path := writeEndpointEditFixture(t, content)
	ok, err := ApplyCityEndpointKeyEditsInPlace(fsys.OSFS{}, path, []CityEndpointKeyEdit{
		{Key: "host", Value: `"other.example.com"`},
	})
	if err != nil {
		t.Fatalf("ApplyCityEndpointKeyEditsInPlace: %v", err)
	}
	if ok {
		t.Fatalf("ok = true, want shape refusal for inline table")
	}
	if got := readEndpointEditFixture(t, path); got != content {
		t.Fatalf("file modified on refusal: %q", got)
	}
}

func TestApplyCityEndpointKeyEditsRefusesMissingFile(t *testing.T) {
	ok, err := ApplyCityEndpointKeyEditsInPlace(fsys.OSFS{}, filepath.Join(t.TempDir(), "city.toml"), []CityEndpointKeyEdit{
		{Key: "host", Value: `"db.example.com"`},
	})
	if err != nil {
		t.Fatalf("ApplyCityEndpointKeyEditsInPlace: %v", err)
	}
	if ok {
		t.Fatalf("ok = true, want refusal for missing file")
	}
}

func TestApplyCityEndpointKeyEditsValueWithHashInsideString(t *testing.T) {
	content := `[dolt]
host = "we#ird" # trailing comment survives
port = 3307
`
	path := writeEndpointEditFixture(t, content)
	ok, err := ApplyCityEndpointKeyEditsInPlace(fsys.OSFS{}, path, []CityEndpointKeyEdit{
		{Key: "host", Value: `"db.example.com"`},
	})
	if err != nil || !ok {
		t.Fatalf("ApplyCityEndpointKeyEditsInPlace = (%v, %v), want (true, nil)", ok, err)
	}
	got := readEndpointEditFixture(t, path)
	if !strings.Contains(got, `host = "db.example.com" # trailing comment survives`) {
		t.Fatalf("trailing comment mangled: %q", got)
	}
}

func decodeEndpointEditConfig(t *testing.T, content string) *City {
	t.Helper()
	cfg, err := decodeCityTOMLForEndpointEdit([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
