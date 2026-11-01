package rig

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/shellquote"
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

// A metadata.json --adopt cannot read refuses the adopt with the read error
// rather than guessing the store's mode, even when config.yaml records the
// prefix.
func TestProvisionAdoptSurfacesAnUnreadableMetadataJSON(t *testing.T) {
	deps, req := adoptStoreFixture(t, "issue_prefix: hv\n")
	metaPath := filepath.Join(req.Path, ".beads", "metadata.json")
	if err := os.Remove(metaPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(metaPath, 0o755); err != nil {
		t.Fatal(err)
	}

	_, _, err := Provision(deps, req)
	want := "--adopt: reading " + metaPath + ": "
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("Provision err = %v, want it to start with %q", err, want)
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

// A mixed-case prefix is refused with the rename to its lowercase form, for
// every store shape: in place for an embedded store, through server mode and
// back for a proxied-server one, and in config.yaml too when it is recorded
// there. A store bd cannot rename is refused as one that cannot be adopted.
func TestProvisionAdoptRefusesAMixedCaseStorePrefix(t *testing.T) {
	for _, rec := range prefixRecords {
		t.Run(rec.name, func(t *testing.T) {
			deps, req := adoptStoreRecordingPrefix(t, "KitFlow", rec)

			_, _, err := Provision(deps, req)
			where := ""
			if rec.inConfigYAML {
				where = " in .beads/config.yaml"
			}
			remedy := "; rename that prefix to kitflow in the store before adopting it: " + recordRenameText(req.Path, "kitflow", rec)
			if rec.externalEndpoint {
				remedy = ", and " + cannotRenameText("this proxied-server store", wontSwitchText(externalEndpointReason)) + ", so it cannot be adopted"
			}
			want := `--adopt: the store at ` + req.Path + ` records issue prefix "KitFlow"` + where + `; gc stores rig prefixes in lowercase, so adopting it would fork its bead ID series` + remedy
			if err == nil || err.Error() != want {
				t.Fatalf("Provision err = %v, want %q", err, want)
			}
			assertProxiedRenameGoesThroughServerMode(t, rec, err)
			assertCityTOMLUntouched(t, deps)
		})
	}
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
			want := `--adopt: the store at ` + req.Path + ` records issue prefix "KitFlow" in .beads/config.yaml; gc stores rig prefixes in lowercase, so adopting it would fork its bead ID series; rename that prefix to kitflow in the store before adopting it: from a shell outside any gc session, run ` + "`BEADS_DIR=" + filepath.Join(req.Path, ".beads") + " bd rename-prefix kitflow`" + `, then change the prefix in .beads/config.yaml to kitflow`
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

// adoptedStoreRemedyText is the remedy an adopt's refusal of its store's own
// prefix names: --prefix must match the store, so neither another --prefix nor
// a [[rigs]] prefix can help; the store's prefix itself has to change
// (recordRenameText). A store bd cannot rename is said to be one that cannot
// be adopted while blocker holds.
func adoptedStoreRemedyText(rigPath string, rec prefixRecord, blocker string) string {
	if rec.externalEndpoint {
		return "an adopted rig keeps its store's own prefix, and " + cannotRenameText("the proxied-server store at "+rigPath, wontSwitchText(externalEndpointReason)) + ", so that store cannot be adopted while " + blocker
	}
	return "an adopted rig keeps its store's own prefix, so rename that prefix in the store before adopting it: " + recordRenameText(rigPath, "<new-prefix>", rec)
}

// recordRenameText is renameText for a store of rec's shape, whose sidecar, for
// a proxied-server store, is gc's (gcProxiedSidecar).
func recordRenameText(rigPath, newPrefix string, rec prefixRecord) string {
	idleFlag := ""
	if rec.proxied {
		idleFlag = gcProxiedSidecarIdleFlag
	}
	return renameText(rigPath, newPrefix, rec.inConfigYAML, rec.proxied, idleFlag)
}

// renameText is how a refusal says to rename the prefix of the store at rigPath
// to newPrefix: bd rename-prefix, from a shell outside any gc session with
// BEADS_DIR naming the store. bd rename-prefix refuses a proxied-server store,
// so such a store is switched to server mode for it and back, given back its
// idle timeout with idleFlag. bd rename-prefix leaves .beads/config.yaml alone
// and --adopt reads a prefix recorded there first, so a store whose
// config.yaml records it (inConfigYAML) is told to change it there too.
func renameText(rigPath, newPrefix string, inConfigYAML, proxied bool, idleFlag string) string {
	beadsDir := filepath.Join(rigPath, ".beads")
	text := "from a shell outside any gc session, run `BEADS_DIR=" + beadsDir + " bd rename-prefix " + newPrefix + "`"
	if proxied {
		text = "bd rename-prefix refuses proxied-server stores, so switch this one to server mode for the rename and back with bd's experimental mode migrations: in a new shell outside any gc session, run `export BEADS_DIR=" + beadsDir + "`, then `bd dolt stop`, `bd migrate from-proxied-server-to-server`, `bd dolt start`, `bd rename-prefix " + newPrefix + "`, `bd dolt stop` and `bd migrate from-server-to-proxied-server" + idleFlag + "`"
	}
	if inConfigYAML {
		text += ", then change the prefix in .beads/config.yaml to " + newPrefix
	}
	return text
}

// cannotRenameText is how a refusal says that bd cannot rename the prefix of
// store, a proxied-server store that cannot go through server mode for the
// rename either, as clause says (wontSwitchText, offRootText).
func cannotRenameText(store, clause string) string {
	return "bd cannot rename the prefix of " + store + ": bd rename-prefix refuses proxied-server stores, and " + clause
}

// wontSwitchText is cannotRenameText's clause for a store bd will not switch
// to server mode because of reason.
func wontSwitchText(reason string) string {
	return "bd will not switch this one to server mode because " + reason
}

// offRootText is cannotRenameText's clause for a store whose Dolt root, root,
// is not its own .beads/dolt.
func offRootText(root string) string {
	return "switching this one to server mode would not help, because its Dolt root " + root + " is not its own .beads/dolt (bd dolt stop would stop the proxy serving every scope on that root, and bd rename-prefix would act on the workspace that owns it)"
}

// assertProxiedRenameGoesThroughServerMode fails when a refusal for a
// proxied-server store names bd rename-prefix anywhere but between the switch
// to server mode and the switch back, where bd refuses it, or names a
// config.yaml change without that rename, which alone would adopt the store
// while its beads keep the old prefix.
func assertProxiedRenameGoesThroughServerMode(t *testing.T, rec prefixRecord, err error) {
	t.Helper()
	if !rec.proxied || err == nil {
		return
	}
	text := err.Error()
	toServer := strings.Index(text, "`bd migrate from-proxied-server-to-server`")
	rename := strings.Index(text, "`bd rename-prefix ")
	back := strings.Index(text, "`bd migrate from-server-to-proxied-server")
	if rename >= 0 && (toServer < 0 || toServer > rename || back < rename) {
		t.Errorf("refusal for a proxied-server store names bd rename-prefix outside the switch to server mode and back: %v", err)
	}
	if strings.Contains(text, "config.yaml to") && rename < 0 {
		t.Errorf("refusal for a proxied-server store names a config.yaml change without the rename: %v", err)
	}
}

// prefixRecord is one shape of store --adopt reads a prefix from.
// inConfigYAML: .beads/config.yaml records the prefix, which every store a gc
// city has managed does and which wins without the store being asked;
// otherwise only the store's own config table does, which is all bd init
// leaves. proxied: metadata.json puts the store in proxied-server mode, gc's
// default transport, where bd rename-prefix refuses to run, and the store has
// the client-info sidecar gc's bd init leaves (gcProxiedSidecar); otherwise it
// is embedded. externalEndpoint: that sidecar names an externally hosted Dolt
// endpoint, so bd will not switch the store to server mode to rename it.
type prefixRecord struct {
	name             string
	inConfigYAML     bool
	proxied          bool
	externalEndpoint bool
}

var prefixRecords = []prefixRecord{
	{name: "recorded by the store only"},
	{name: "recorded in config.yaml", inConfigYAML: true},
	{name: "recorded by a proxied-server store only", proxied: true},
	{name: "recorded in a proxied-server store's config.yaml", inConfigYAML: true, proxied: true},
	{name: "recorded in the config.yaml of a proxied-server store on an external endpoint", inConfigYAML: true, proxied: true, externalEndpoint: true},
}

// proxiedStoreMetadata is the metadata.json bd init --proxied-server writes.
const proxiedStoreMetadata = `{"backend":"dolt","dolt_mode":"proxied-server","dolt_database":"hv"}`

// gcProxiedSidecar is the client-info sidecar bd init writes for gc, which
// always passes --proxied-server-idle-timeout: here gc's default, 30m.
// gcProxiedSidecarIdleFlag is the --idle-timeout that gives it back.
// gcExternalProxiedSidecar is the one it writes for a store whose data is on
// an externally hosted Dolt endpoint, which bd will not switch to server mode
// (externalEndpointReason).
const (
	gcProxiedSidecar         = `{"idle_timeout":1800000000000}`
	gcProxiedSidecarIdleFlag = " --idle-timeout 30m0s"
	gcExternalProxiedSidecar = `{"idle_timeout":1800000000000,"external":{"host":"dolt.example.com","port":3307}}`
	externalEndpointReason   = "its .beads/proxied_server_client_info.json names an externally hosted Dolt endpoint"
)

// adoptStoreRecordingPrefix is adoptStoreFixture for a store of rec's shape
// that records prefix. A config.yaml store's reader fails the test if it is
// asked.
func adoptStoreRecordingPrefix(t *testing.T, prefix string, rec prefixRecord) (Deps, ProvisionRequest) {
	t.Helper()
	configYAML := bdInitConfigYAML
	if rec.inConfigYAML {
		configYAML = "issue_prefix: " + prefix + "\n"
	}
	deps, req := adoptStoreFixture(t, configYAML)
	if rec.proxied {
		beadsDir := filepath.Join(req.Path, ".beads")
		if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(proxiedStoreMetadata), 0o644); err != nil {
			t.Fatal(err)
		}
		sidecar := gcProxiedSidecar
		if rec.externalEndpoint {
			sidecar = gcExternalProxiedSidecar
		}
		if err := os.WriteFile(filepath.Join(beadsDir, "proxied_server_client_info.json"), []byte(sidecar), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if !rec.inConfigYAML {
		deps.ReadStorePrefix = (&storePrefixStub{}).reader(prefix, nil)
		return deps, req
	}
	deps.ReadStorePrefix = func(string) (string, error) {
		t.Error("ReadStorePrefix consulted although config.yaml records the prefix")
		return prefix, nil
	}
	return deps, req
}

func TestProvisionAdoptStorePrefixCollisionNamesTheStoreRemedy(t *testing.T) {
	for _, rec := range prefixRecords {
		for _, tc := range []struct {
			name    string
			cfg     *config.City
			other   string
			blocker string
			prefix  string
		}{
			{name: "HQ without --prefix", cfg: &config.City{Workspace: config.Workspace{Name: "havana", Prefix: "hv"}}, other: "HQ", blocker: "HQ holds that prefix"},
			{name: "HQ with the store's prefix as --prefix", cfg: &config.City{Workspace: config.Workspace{Name: "havana", Prefix: "hv"}}, other: "HQ", blocker: "HQ holds that prefix", prefix: "hv"},
			{name: "rig without --prefix", cfg: &config.City{Rigs: []config.Rig{{Name: "first", Path: "/elsewhere/first", Prefix: "hv"}}}, other: "first", blocker: `rig "first" holds that prefix`},
			{name: "rig with the store's prefix as --prefix", cfg: &config.City{Rigs: []config.Rig{{Name: "first", Path: "/elsewhere/first", Prefix: "hv"}}}, other: "first", blocker: `rig "first" holds that prefix`, prefix: "hv"},
			{name: "rig city.toml records no path for", cfg: &config.City{Rigs: []config.Rig{{Name: "first", Prefix: "hv"}}}, other: "first", blocker: `rig "first" holds that prefix`},
		} {
			t.Run(rec.name+"/"+tc.name, func(t *testing.T) {
				deps, req := adoptStoreRecordingPrefix(t, "hv", rec)
				deps.Cfg = tc.cfg
				req.Prefix = tc.prefix

				_, _, err := Provision(deps, req)
				want := `rig "webapp": prefix "hv" collides with ` + tc.other + "; " + adoptedStoreRemedyText(req.Path, rec, tc.blocker)
				if err == nil || err.Error() != want {
					t.Fatalf("Provision err = %v, want %q", err, want)
				}
				assertProxiedRenameGoesThroughServerMode(t, rec, err)
				assertCityTOMLUntouched(t, deps)
			})
		}
	}
}

// proxiedStoreShape is a proxied-server store whose prefix bd does, or does
// not, rename. bd rename-prefix refuses proxied-server mode, so the rename
// switches the store to server mode and back. bd migrate
// from-proxied-server-to-server refuses a store with no client-info sidecar,
// one whose sidecar it cannot read or that names an externally hosted Dolt
// endpoint, and one metadata.json marks dolt_team_server. A store whose Dolt
// root, under its sidecar's root_path or else at its metadata.json
// dolt_data_dir (dataDir), is not its own .beads/dolt is not switched either.
// stuck is the clause that says why (cannotRenameText). On the way back, bd
// migrate from-server-to-proxied-server takes the --idle-timeout that gives the
// store back the one its sidecar records (idleFlag); bd's default needs none.
type proxiedStoreShape struct {
	name       string
	sidecar    string // the sidecar's content; "" for none
	teamServer bool
	dataDir    string // metadata.json's dolt_data_dir; "" for none
	idleFlag   string
	stuck      string
}

// proxiedStoreShapes are the shapes
// TestProvisionAdoptProxiedStoreRemedyFollowsWhatBdWillMigrate adopts.
func proxiedStoreShapes() []proxiedStoreShape {
	notJSON := json.Unmarshal([]byte("{"), &struct{}{})
	return []proxiedStoreShape{
		{name: "gc's idle timeout", sidecar: gcProxiedSidecar, idleFlag: gcProxiedSidecarIdleFlag},
		{name: "an idle timeout of never", sidecar: `{"idle_timeout":-1}`, idleFlag: " --idle-timeout 0"},
		{name: "bd's default idle timeout", sidecar: `{"root_path":"dolt"}`},
		{name: "a dolt_data_dir naming its own .beads/dolt", sidecar: gcProxiedSidecar, dataDir: "dolt", idleFlag: gcProxiedSidecarIdleFlag},
		{name: "a root_path naming its own .beads/dolt over a dolt_data_dir outside it", sidecar: `{"root_path":"dolt"}`, dataDir: "/srv/city/.beads/dolt"},
		{name: "no sidecar", stuck: wontSwitchText("it has no .beads/proxied_server_client_info.json")},
		{name: "an externally hosted endpoint", sidecar: gcExternalProxiedSidecar, stuck: wontSwitchText(externalEndpointReason)},
		{name: "a sidecar that is not JSON", sidecar: "{", stuck: wontSwitchText("its .beads/proxied_server_client_info.json cannot be read (" + notJSON.Error() + ")")},
		{name: "a team-server store", sidecar: gcProxiedSidecar, teamServer: true, stuck: wontSwitchText("its .beads/metadata.json sets dolt_team_server")},
		{name: "a root_path outside its .beads", sidecar: `{"root_path":"/srv/city/.beads/dolt","idle_timeout":1800000000000}`, stuck: offRootText("/srv/city/.beads/dolt")},
		{name: "a root_path outside its .beads over a dolt_data_dir naming its own .beads/dolt", sidecar: `{"root_path":"/srv/city/.beads/dolt","idle_timeout":1800000000000}`, dataDir: "dolt", stuck: offRootText("/srv/city/.beads/dolt")},
		{name: "a dolt_data_dir outside its .beads", sidecar: gcProxiedSidecar, dataDir: "/srv/city/.beads/dolt", stuck: offRootText("/srv/city/.beads/dolt")},
	}
}

// adoptProxiedStoreOfShape is adoptStoreRecordingPrefix for a proxied-server
// store of shape that records prefix, in config.yaml when inConfigYAML.
func adoptProxiedStoreOfShape(t *testing.T, prefix string, inConfigYAML bool, shape proxiedStoreShape) (Deps, ProvisionRequest) {
	t.Helper()
	deps, req := adoptStoreRecordingPrefix(t, prefix, prefixRecord{inConfigYAML: inConfigYAML, proxied: true})
	beadsDir := filepath.Join(req.Path, ".beads")
	sidecarPath := filepath.Join(beadsDir, "proxied_server_client_info.json")
	if shape.sidecar == "" {
		if err := os.Remove(sidecarPath); err != nil {
			t.Fatal(err)
		}
	} else if err := os.WriteFile(sidecarPath, []byte(shape.sidecar), 0o644); err != nil {
		t.Fatal(err)
	}
	if shape.teamServer || shape.dataDir != "" {
		meta := map[string]any{"backend": "dolt", "dolt_mode": "proxied-server", "dolt_database": "hv"}
		if shape.teamServer {
			meta["dolt_team_server"] = true
		}
		if shape.dataDir != "" {
			meta["dolt_data_dir"] = shape.dataDir
		}
		data, err := json.Marshal(meta)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return deps, req
}

// A proxied-server store's refusals name bd's route through server mode when
// bd will switch the store there, giving the store back its idle timeout, and
// say why its prefix cannot be renamed when bd will not.
func TestProvisionAdoptProxiedStoreRemedyFollowsWhatBdWillMigrate(t *testing.T) {
	for _, shape := range proxiedStoreShapes() {
		t.Run(shape.name+"/colliding config.yaml prefix", func(t *testing.T) {
			deps, req := adoptProxiedStoreOfShape(t, "hv", true, shape)
			deps.Cfg = &config.City{Workspace: config.Workspace{Name: "havana", Prefix: "hv"}}

			_, _, err := Provision(deps, req)
			remedy := "so rename that prefix in the store before adopting it: " + renameText(req.Path, "<new-prefix>", true, true, shape.idleFlag)
			if shape.stuck != "" {
				remedy = "and " + cannotRenameText("the proxied-server store at "+req.Path, shape.stuck) + ", so that store cannot be adopted while HQ holds that prefix"
			}
			want := `rig "webapp": prefix "hv" collides with HQ; an adopted rig keeps its store's own prefix, ` + remedy
			if err == nil || err.Error() != want {
				t.Fatalf("Provision err = %v, want %q", err, want)
			}
			assertProxiedRenameGoesThroughServerMode(t, prefixRecord{inConfigYAML: true, proxied: true}, err)
			assertCityTOMLUntouched(t, deps)
		})
		t.Run(shape.name+"/mixed-case store prefix", func(t *testing.T) {
			deps, req := adoptProxiedStoreOfShape(t, "KitFlow", false, shape)

			_, _, err := Provision(deps, req)
			remedy := "; rename that prefix to kitflow in the store before adopting it: " + renameText(req.Path, "kitflow", false, true, shape.idleFlag)
			if shape.stuck != "" {
				remedy = ", and " + cannotRenameText("this proxied-server store", shape.stuck) + ", so it cannot be adopted"
			}
			want := `--adopt: the store at ` + req.Path + ` records issue prefix "KitFlow"; gc stores rig prefixes in lowercase, so adopting it would fork its bead ID series` + remedy
			if err == nil || err.Error() != want {
				t.Fatalf("Provision err = %v, want %q", err, want)
			}
			assertProxiedRenameGoesThroughServerMode(t, prefixRecord{proxied: true}, err)
			assertCityTOMLUntouched(t, deps)
		})
	}
}

// A rig gc beads city migrate-proxied pointed at the city's root stands on
// that root: a dolt_data_dir relative to its .beads and the absolute root_path
// bd's migration writes into its sidecar. bd dolt stop through such a store
// stops the proxy of every scope on the root, so its refusal names the root
// and prints no route. A root_path naming the store's own .beads/dolt, however
// spelled, keeps the route; bd's return migration writes one.
func TestProvisionAdoptProxiedStoreRouteFollowsItsDoltRoot(t *testing.T) {
	writeMetadataDataDir := func(t *testing.T, beadsDir, dataDir string) {
		t.Helper()
		meta := `{"backend":"dolt","dolt_mode":"proxied-server","dolt_database":"hv","dolt_data_dir":"` + dataDir + `"}`
		if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeSidecarRoot := func(t *testing.T, beadsDir, root string) {
		t.Helper()
		sidecar := `{"root_path":"` + root + `","idle_timeout":1800000000000}`
		if err := os.WriteFile(filepath.Join(beadsDir, "proxied_server_client_info.json"), []byte(sidecar), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cityRoot := func(t *testing.T, beadsDir string) (root, rel string) {
		t.Helper()
		root = filepath.Join(t.TempDir(), "city", ".beads", "dolt")
		if err := os.MkdirAll(filepath.Join(root, "hv", ".dolt"), 0o755); err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(beadsDir, root)
		if err != nil {
			t.Fatal(err)
		}
		return root, filepath.ToSlash(rel)
	}
	for _, tc := range []struct {
		name string
		// layout points the store in beadsDir at a Dolt root and returns the
		// root its refusal names, "" for the store's own.
		layout func(t *testing.T, beadsDir string) string
	}{
		{
			name: "the city's root, as gc beads city migrate-proxied leaves a rig",
			layout: func(t *testing.T, beadsDir string) string {
				root, rel := cityRoot(t, beadsDir)
				writeMetadataDataDir(t, beadsDir, rel)
				writeSidecarRoot(t, beadsDir, root)
				return root
			},
		},
		{
			name: "the city's root its relative dolt_data_dir names",
			layout: func(t *testing.T, beadsDir string) string {
				root, rel := cityRoot(t, beadsDir)
				writeMetadataDataDir(t, beadsDir, rel)
				return root
			},
		},
		{
			name: "its own .beads/dolt, spelled through a symlink",
			layout: func(t *testing.T, beadsDir string) string {
				alias := symlinkTo(t, "", filepath.Dir(beadsDir))
				writeSidecarRoot(t, beadsDir, filepath.Join(alias, ".beads", "dolt"))
				return ""
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, req := adoptStoreRecordingPrefix(t, "hv", prefixRecord{inConfigYAML: true, proxied: true})
			deps.Cfg = &config.City{Workspace: config.Workspace{Name: "havana", Prefix: "hv"}}
			root := tc.layout(t, filepath.Join(req.Path, ".beads"))

			_, _, err := Provision(deps, req)
			remedy := "so rename that prefix in the store before adopting it: " + renameText(req.Path, "<new-prefix>", true, true, gcProxiedSidecarIdleFlag)
			if root != "" {
				remedy = "and " + cannotRenameText("the proxied-server store at "+req.Path, offRootText(root)) + ", so that store cannot be adopted while HQ holds that prefix"
			}
			want := `rig "webapp": prefix "hv" collides with HQ; an adopted rig keeps its store's own prefix, ` + remedy
			if err == nil || err.Error() != want {
				t.Fatalf("Provision err = %v, want %q", err, want)
			}
			if root != "" && strings.Contains(err.Error(), "bd dolt stop`") {
				t.Errorf("refusal for a store on another root prints bd dolt stop: %v", err)
			}
			assertCityTOMLUntouched(t, deps)
		})
	}
}

// The printed commands shell-quote the store's path, so they run as printed
// for a rig path with a space and a single quote in it: the shell reads the
// BEADS_DIR assignment as one word naming the store.
func TestProvisionAdoptRemedyShellQuotesTheStorePath(t *testing.T) {
	for _, rec := range prefixRecords {
		if rec.externalEndpoint {
			continue
		}
		t.Run(rec.name, func(t *testing.T) {
			deps, req := adoptStoreRecordingPrefix(t, "hv", rec)
			root := t.TempDir()
			rigPath := filepath.Join(root, "adopt remedy's", "webapp")
			if err := os.Mkdir(filepath.Dir(rigPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(req.Path, rigPath); err != nil {
				t.Fatal(err)
			}
			req.Path = rigPath
			deps.Cfg = &config.City{Workspace: config.Workspace{Name: "havana", Prefix: "hv"}}

			_, _, err := Provision(deps, req)
			beadsDir := filepath.Join(rigPath, ".beads")
			quoted := "BEADS_DIR='" + root + `/adopt remedy'\''s/webapp/.beads'`
			want := `rig "webapp": prefix "hv" collides with HQ; ` + strings.Replace(adoptedStoreRemedyText(rigPath, rec, "HQ holds that prefix"), "BEADS_DIR="+beadsDir, quoted, 1)
			if err == nil || err.Error() != want {
				t.Fatalf("Provision err = %v, want %q", err, want)
			}
			var assignments []string
			for i, part := range strings.Split(err.Error(), "`") {
				if i%2 == 0 {
					continue
				}
				for _, word := range shellquote.Split(part) {
					if strings.HasPrefix(word, "BEADS_DIR=") {
						assignments = append(assignments, word)
					}
				}
			}
			if wantAssignments := []string{"BEADS_DIR=" + beadsDir}; !slices.Equal(assignments, wantAssignments) {
				t.Errorf("the shell reads the printed BEADS_DIR assignments as %q, want %q", assignments, wantAssignments)
			}
			assertCityTOMLUntouched(t, deps)
		})
	}
}

// symlinkTo returns a new symlink to dir, another spelling of the same
// directory.
func symlinkTo(t *testing.T, _, dir string) string {
	t.Helper()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	return alias
}

// asSpelled returns dir as spell spells it, or unchanged when spell is nil.
func asSpelled(t *testing.T, spell func(t *testing.T, cityPath, dir string) string, cityPath, dir string) string {
	t.Helper()
	if spell == nil {
		return dir
	}
	return spell(t, cityPath, dir)
}

// An --adopt of the very store that already holds the colliding prefix, the
// city's own or a registered rig's, is refused as what it is, however either
// side spells its directory: no rename is named, because renaming that store's
// prefix would break the scope using it.
func TestProvisionAdoptOfAStoreTheCityAlreadyUsesSaysSo(t *testing.T) {
	t.Run("the city's own store", func(t *testing.T) {
		for _, rec := range prefixRecords {
			for _, tc := range []struct {
				name    string
				adopted func(t *testing.T, cityPath, dir string) string
			}{
				{name: "at its path"},
				{name: "through a symlink to it", adopted: symlinkTo},
			} {
				t.Run(rec.name+"/"+tc.name, func(t *testing.T) {
					deps, req := adoptStoreRecordingPrefix(t, "hv", rec)
					if err := os.Rename(filepath.Join(req.Path, ".beads"), filepath.Join(deps.CityPath, ".beads")); err != nil {
						t.Fatal(err)
					}
					req.Name, req.Path = "hq-again", asSpelled(t, tc.adopted, deps.CityPath, deps.CityPath)
					deps.Cfg = &config.City{Workspace: config.Workspace{Name: "havana", Prefix: "hv"}}

					_, _, err := Provision(deps, req)
					want := `rig "hq-again": --adopt: the store at ` + req.Path + ` is the city's own HQ store; it cannot also be registered as a rig`
					if err == nil || err.Error() != want {
						t.Fatalf("Provision err = %v, want %q", err, want)
					}
					assertCityTOMLUntouched(t, deps)
				})
			}
		}
	})
	t.Run("a registered rig's store", func(t *testing.T) {
		for _, rec := range prefixRecords {
			for _, tc := range []struct {
				name string
				// registered and adopted spell the store's directory as
				// city.toml records the rig's path and as the adopt names it.
				registered, adopted func(t *testing.T, cityPath, dir string) string
			}{
				{name: "at its absolute path"},
				{name: "at an unclean absolute path", registered: func(_ *testing.T, _, dir string) string { return dir + "/sub/.." }},
				{name: "at a city-relative path", registered: func(t *testing.T, cityPath, dir string) string {
					rel, err := filepath.Rel(cityPath, dir)
					if err != nil {
						t.Fatal(err)
					}
					return rel
				}},
				{name: "registered at a symlink to it", registered: symlinkTo},
				{name: "adopted through a symlink to it", adopted: symlinkTo},
			} {
				t.Run(rec.name+"/"+tc.name, func(t *testing.T) {
					deps, req := adoptStoreRecordingPrefix(t, "hv", rec)
					deps.Cfg = &config.City{Rigs: []config.Rig{{Name: "first", Path: asSpelled(t, tc.registered, deps.CityPath, req.Path), Prefix: "hv"}}}
					req.Path = asSpelled(t, tc.adopted, deps.CityPath, req.Path)

					_, _, err := Provision(deps, req)
					want := `rig "webapp": --adopt: the store at ` + req.Path + ` is already registered as rig "first"`
					if err == nil || err.Error() != want {
						t.Fatalf("Provision err = %v, want %q", err, want)
					}
					assertCityTOMLUntouched(t, deps)
				})
			}
		}
	})
}

// An --adopt whose --prefix differs from the store's but itself collides is
// not refused over the store's prefix, so it keeps the fresh-add text.
func TestProvisionAdoptMismatchingCollidingPrefixKeepsTheFreshAddText(t *testing.T) {
	deps, req := adoptStoreFixture(t, bdInitConfigYAML)
	deps.Cfg = &config.City{Workspace: config.Workspace{Name: "havana", Prefix: "zz"}}
	deps.ReadStorePrefix = (&storePrefixStub{}).reader("hv", nil)
	req.Prefix = "zz"

	_, _, err := Provision(deps, req)
	want := `rig "webapp": prefix "zz" collides with HQ. Use --prefix to specify a different prefix.`
	if err == nil || err.Error() != want {
		t.Fatalf("Provision err = %v, want %q", err, want)
	}
	assertCityTOMLUntouched(t, deps)
}

func TestProvisionFreshAddCollisionKeepsThePrefixRemedy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cfg   *config.City
		other string
	}{
		{name: "HQ", cfg: &config.City{Workspace: config.Workspace{Name: "havana", Prefix: "hv"}}, other: "HQ"},
		{name: "rig", cfg: &config.City{Rigs: []config.Rig{{Name: "first", Path: "/elsewhere/first", Prefix: "hv"}}}, other: "first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := stubDeps(t.TempDir())
			deps.Cfg = tc.cfg
			_, _, err := Provision(deps, ProvisionRequest{Name: "webapp", Path: filepath.Join(t.TempDir(), "webapp"), Prefix: "hv"})
			want := `rig "webapp": prefix "hv" collides with ` + tc.other + `. Use --prefix to specify a different prefix.`
			if err == nil || err.Error() != want {
				t.Fatalf("Provision err = %v, want %q", err, want)
			}
		})
	}
}

func TestProvisionAdoptReservedStorePrefixOnASplitCityNamesTheStoreRemedy(t *testing.T) {
	for _, rec := range prefixRecords {
		t.Run(rec.name, func(t *testing.T) {
			deps, req := adoptStoreRecordingPrefix(t, "gco", rec)
			deps.Cfg = splitCityCfg()

			_, _, err := Provision(deps, req)
			if err == nil {
				t.Fatal("adopting a store with a reserved prefix on a split city succeeded")
			}
			// The adopted rig's is the only problem, so ". " ends it.
			wantProblem := `rig "webapp" bead prefix "gco" is in the reserved "gco" namespace; ` + adoptedStoreRemedyText(req.Path, rec, "this city reserves that namespace") + ". "
			if !strings.Contains(err.Error(), wantProblem) {
				t.Errorf("refusal %q does not contain %q", err, wantProblem)
			}
			if strings.Contains(err.Error(), "[[rigs]] entry") {
				t.Errorf("refusal names a [[rigs]] prefix remedy an adopt cannot use: %v", err)
			}
			assertProxiedRenameGoesThroughServerMode(t, rec, err)
			assertCityTOMLUntouched(t, deps)
		})
	}
}

// A fresh add's reserved prefix keeps the [[rigs]] remedy: that rig's prefix
// is gc's to choose.
func TestProvisionFreshAddReservedPrefixOnASplitCityKeepsTheRigsRemedy(t *testing.T) {
	deps := stubDeps(t.TempDir())
	deps.Cfg = splitCityCfg()
	_, _, err := Provision(deps, ProvisionRequest{Name: "beta", Path: filepath.Join(t.TempDir(), "beta"), Prefix: "gco"})
	want := `rig "beta" bead prefix "gco" is in the reserved "gco" namespace; set an explicit prefix: prefix = "<prefix>" on that [[rigs]] entry`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Provision err = %v, want it to contain %q", err, want)
	}
}
