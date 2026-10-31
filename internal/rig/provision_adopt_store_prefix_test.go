package rig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// bd (v0.48+) keeps a store's issue_prefix only in the database's config
// table: `bd init` leaves .beads/config.yaml as a commented template. These
// tests pin that --adopt reads the prefix back from the store when
// config.yaml records none, verifies --prefix against it, and defaults to it.

// bdInitConfigYAML is the shape bd init leaves behind: no live keys.
const bdInitConfigYAML = "# Beads configuration\n# issue-prefix: \"\"\n# sync-branch: \"\"\n"

// adoptStoreFixture builds a city with a city.toml and an adoptable rig
// directory named "webapp" (which derives the prefix "we") whose store was
// created by bd init: metadata.json present, config.yaml without a prefix.
func adoptStoreFixture(t *testing.T, configYAML string) (Deps, ProvisionRequest) {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(originalCityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	rigPath := filepath.Join(t.TempDir(), "webapp")
	beadsDir := filepath.Join(rigPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"backend":"dolt","dolt_mode":"embedded","dolt_database":"hv"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := stubDeps(cityPath)
	deps.NormalizeScopes = func(string, *config.City) error { return nil }
	return deps, ProvisionRequest{Name: "webapp", Path: rigPath, Adopt: true}
}

// storePrefixStub records the rig paths it is asked about.
type storePrefixStub struct {
	calls []string
}

func (s *storePrefixStub) reader(prefix string, err error) func(string) (string, error) {
	return func(rigPath string) (string, error) {
		s.calls = append(s.calls, rigPath)
		return prefix, err
	}
}

func readCityTOML(t *testing.T, deps Deps) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(deps.CityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertCityTOMLUntouched(t *testing.T, deps Deps) {
	t.Helper()
	if got := readCityTOML(t, deps); got != originalCityTOML {
		t.Fatalf("a refused adopt rewrote city.toml:\n%s", got)
	}
}

func TestProvisionAdoptTakesPrefixFromStoreWhenConfigYamlHasNone(t *testing.T) {
	deps, req := adoptStoreFixture(t, bdInitConfigYAML)
	stub := &storePrefixStub{}
	deps.ReadStorePrefix = stub.reader("hv", nil)

	_, res, err := Provision(deps, req)
	if err != nil {
		t.Fatalf("adopting a bd-init store failed: %v", err)
	}
	if len(stub.calls) != 1 || stub.calls[0] != req.Path {
		t.Fatalf("ReadStorePrefix calls = %v, want exactly [%s]", stub.calls, req.Path)
	}
	var sawPrefix bool
	for _, step := range res.Steps {
		if step.Detail == "  Prefix: hv" {
			sawPrefix = true
		}
	}
	if !sawPrefix {
		t.Errorf("progress never announced the store's prefix: %+v", res.Steps)
	}
	if got := readCityTOML(t, deps); !strings.Contains(got, `prefix = "hv"`) {
		t.Errorf("city.toml does not pin the adopted store's prefix:\n%s", got)
	}
}

func TestProvisionAdoptRefusesAPrefixTheStoreDoesNotRecord(t *testing.T) {
	deps, req := adoptStoreFixture(t, bdInitConfigYAML)
	deps.ReadStorePrefix = (&storePrefixStub{}).reader("hv", nil)
	req.Prefix = "zz"

	_, _, err := Provision(deps, req)
	want := `--adopt: rig "webapp" already has bead prefix "hv" (requested "zz"); use --prefix hv (or omit --prefix) to match the existing store`
	if err == nil || err.Error() != want {
		t.Fatalf("Provision err = %v, want %q", err, want)
	}
	assertCityTOMLUntouched(t, deps)
}

func TestProvisionAdoptSurfacesAStoreReadFailure(t *testing.T) {
	deps, req := adoptStoreFixture(t, bdInitConfigYAML)
	readErr := errors.New("bd: no beads database found")
	deps.ReadStorePrefix = (&storePrefixStub{}).reader("", readErr)

	_, _, err := Provision(deps, req)
	if err == nil || !errors.Is(err, readErr) {
		t.Fatalf("Provision err = %v, want it to wrap %v", err, readErr)
	}
	if want := "--adopt requires a valid issue_prefix in .beads/config.yaml in "; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("error %q does not start with %q", err, want)
	}
	assertCityTOMLUntouched(t, deps)
}

func TestProvisionAdoptStoreRecordingNoPrefixKeepsTheRefusal(t *testing.T) {
	deps, req := adoptStoreFixture(t, bdInitConfigYAML)
	deps.ReadStorePrefix = (&storePrefixStub{}).reader("", nil)

	_, _, err := Provision(deps, req)
	want := "--adopt requires a valid issue_prefix in .beads/config.yaml in " + req.Path
	if err == nil || err.Error() != want {
		t.Fatalf("Provision err = %v, want %q", err, want)
	}
	assertCityTOMLUntouched(t, deps)
}

func TestProvisionAdoptRefusesAMixedCaseStorePrefix(t *testing.T) {
	deps, req := adoptStoreFixture(t, bdInitConfigYAML)
	deps.ReadStorePrefix = (&storePrefixStub{}).reader("KitFlow", nil)

	_, _, err := Provision(deps, req)
	want := `--adopt: the store at ` + req.Path + ` records issue prefix "KitFlow"; gc stores rig prefixes in lowercase, so adopting it would fork its bead ID series; run "bd rename-prefix kitflow" in ` + req.Path + ` first`
	if err == nil || err.Error() != want {
		t.Fatalf("Provision err = %v, want %q", err, want)
	}
	assertCityTOMLUntouched(t, deps)
}

// A mixed-case prefix in config.yaml is refused under the same rule as one in
// the store, with or without --prefix and on a re-add (whose adopt still runs
// store init with the lowercased prefix): lowercasing it would pin a prefix the
// store's beads do not carry. bd rename-prefix leaves config.yaml alone, so
// the remedy names that file too.
func TestProvisionAdoptRefusesAMixedCaseConfigYamlPrefix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix string
		reAdd  bool
	}{
		{name: "without --prefix"},
		{name: "with the lowercased --prefix", prefix: "kitflow"},
		{name: "re-adding a rig city.toml pins lowercased", reAdd: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, req := adoptStoreFixture(t, "issue_prefix: KitFlow\n")
			stub := &storePrefixStub{}
			deps.ReadStorePrefix = stub.reader("kitflow", nil)
			req.Prefix = tc.prefix
			if tc.reAdd {
				deps.Cfg = &config.City{Rigs: []config.Rig{{Name: "webapp", Path: req.Path, Prefix: "kitflow"}}}
			}

			_, _, err := Provision(deps, req)
			want := `--adopt: the store at ` + req.Path + ` records issue prefix "KitFlow" in .beads/config.yaml; gc stores rig prefixes in lowercase, so adopting it would fork its bead ID series; run "bd rename-prefix kitflow" in ` + req.Path + ` first and change the prefix in .beads/config.yaml to kitflow`
			if err == nil || err.Error() != want {
				t.Fatalf("Provision err = %v, want %q", err, want)
			}
			if len(stub.calls) != 0 {
				t.Errorf("ReadStorePrefix consulted although config.yaml records the prefix: %v", stub.calls)
			}
			assertCityTOMLUntouched(t, deps)
		})
	}
}

func TestProvisionAdoptPrefersConfigYamlPrefix(t *testing.T) {
	deps, req := adoptStoreFixture(t, "issue_prefix: xr\n")
	deps.ReadStorePrefix = func(string) (string, error) {
		t.Fatal("ReadStorePrefix consulted although config.yaml records the prefix")
		return "", nil
	}

	if _, _, err := Provision(deps, req); err != nil {
		t.Fatalf("adopting a store whose config.yaml records its prefix failed: %v", err)
	}
	if got := readCityTOML(t, deps); !strings.Contains(got, `prefix = "xr"`) {
		t.Errorf("city.toml does not pin config.yaml's prefix:\n%s", got)
	}
}

func TestProvisionAdoptWithoutMetadataNeverReadsTheStore(t *testing.T) {
	deps, req := adoptStoreFixture(t, bdInitConfigYAML)
	if err := os.Remove(filepath.Join(req.Path, ".beads", "metadata.json")); err != nil {
		t.Fatal(err)
	}
	stub := &storePrefixStub{}
	deps.ReadStorePrefix = stub.reader("hv", nil)

	_, _, err := Provision(deps, req)
	want := "--adopt requires .beads/metadata.json in " + req.Path
	if err == nil || err.Error() != want {
		t.Fatalf("Provision err = %v, want %q", err, want)
	}
	if len(stub.calls) != 0 {
		t.Errorf("ReadStorePrefix called without metadata.json: %v", stub.calls)
	}
}

func TestProvisionWithoutAdoptNeverReadsTheStore(t *testing.T) {
	t.Run("fresh add", func(t *testing.T) {
		deps := stubDeps(t.TempDir())
		deps.NormalizeScopes = func(string, *config.City) error { return nil }
		if err := os.WriteFile(filepath.Join(deps.CityPath, "city.toml"), []byte(originalCityTOML), 0o644); err != nil {
			t.Fatal(err)
		}
		stub := &storePrefixStub{}
		deps.ReadStorePrefix = stub.reader("hv", nil)
		if _, _, err := Provision(deps, ProvisionRequest{Name: "fresh", Path: filepath.Join(t.TempDir(), "fresh"), Prefix: "fr"}); err != nil {
			t.Fatalf("fresh add failed: %v", err)
		}
		if len(stub.calls) != 0 {
			t.Errorf("ReadStorePrefix called on a fresh add: %v", stub.calls)
		}
	})
	t.Run("re-add", func(t *testing.T) {
		deps, req := adoptStoreFixture(t, bdInitConfigYAML)
		req.Adopt = false
		deps.Cfg = &config.City{Rigs: []config.Rig{{Name: "webapp", Path: req.Path, Prefix: "hv"}}}
		stub := &storePrefixStub{}
		deps.ReadStorePrefix = stub.reader("hv", nil)
		if _, _, err := Provision(deps, req); err != nil {
			t.Fatalf("re-add failed: %v", err)
		}
		if len(stub.calls) != 0 {
			t.Errorf("ReadStorePrefix called on a re-add: %v", stub.calls)
		}
	})
}

func TestProvisionAdoptStorePrefixMeetsTheSplitReservedRefusal(t *testing.T) {
	deps, req := adoptStoreFixture(t, bdInitConfigYAML)
	deps.Cfg = splitCityCfg()
	deps.ReadStorePrefix = (&storePrefixStub{}).reader("gco", nil)

	_, _, err := Provision(deps, req)
	if err == nil || !strings.Contains(err.Error(), "reserved") || !strings.Contains(err.Error(), "gco") {
		t.Fatalf("Provision err = %v, want the reserved-prefix refusal naming gco", err)
	}
	assertCityTOMLUntouched(t, deps)
}
