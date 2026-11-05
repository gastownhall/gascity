//go:build acceptance_a

package acceptance_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// initProxiedSingleStore keeps recorder and legacy-storage controls on an
// explicitly selected single-store topology. The proxied/local selector keeps
// the factory pack while excluding automatic infrastructure separation.
// Ordinary no-selector default coverage uses the real bd separately.
func initProxiedSingleStore(t *testing.T, city *helpers.City, noStart bool) {
	t.Helper()
	flags := []string{"--beads-transport", "proxied", "--beads-target", "local"}
	if noStart {
		city.InitNoStart("claude", flags...)
	} else {
		city.Init("claude", flags...)
	}
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(city.Dir, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.EffectiveStorage().Equal((&config.City{}).EffectiveStorage()) {
		t.Fatalf("recorder control has relocated storage classes: %+v", cfg.EffectiveStorage())
	}
}

// The ordinary factory default must remain covered separately from the
// explicitly single-store recording controls. This uses bd's real Go binary,
// preserves the public factory/default-rig imports, and retires both the work
// proxy and the independently owned infrastructure server.
func TestBeadsFactoryDefaultStorageFrontDoors(t *testing.T) {
	bdPath, doltPath := requireProxiedTooling(t)
	env := proxiedEnv(t, bdPath, doltPath).WithRealBD(bdPath)
	rig := createNamedGitRig(t, "factory-rig")
	city := helpers.NewCity(t, env)
	t.Cleanup(func() {
		stopIfPairsRemain(t, env, city.Dir, city.Dir, rig)
	})
	city.Init("claude") // No topology selector: exercise the actual default.
	assertProxiedScope(t, env, city.Dir, "factory work store")
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(city.Dir, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	storage := cfg.EffectiveStorage()
	if storage.Classes.Work == storage.Classes.Graph || storage.Bindings[storage.Classes.Graph].Provider != config.StorageProviderBeadsWorkspace {
		t.Fatalf("ordinary factory did not separate native infrastructure: %+v", storage)
	}
	infra := filepath.Join(city.Dir, ".gc", "storage", "infra")
	var journal scopeOwnershipDoc
	readJSONFile(t, filepath.Join(city.Dir, ".gc", "scope-ownership.json"), &journal)
	infraOwned := false
	for _, entry := range journal.Scopes {
		if entry.ScopePath == infra && entry.LifecycleOwner == "provider" && entry.State == "ready" {
			infraOwned = true
		}
	}
	if !infraOwned {
		t.Fatalf("factory infrastructure lacks its own ready provider ownership: %+v", journal)
	}
	var infraMetadata proxiedBeadsMetadata
	readJSONFile(t, filepath.Join(infra, ".beads", "metadata.json"), &infraMetadata)
	if infraMetadata.DoltMode != "server" {
		t.Fatalf("infrastructure is not provider-owned direct storage: %+v", infraMetadata)
	}
	pack, err := os.ReadFile(filepath.Join(city.Dir, "pack.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pack), config.PublicGascityPackSource) || !strings.Contains(string(pack), config.PublicGascityPackVersion) {
		t.Fatal("ordinary factory lost its pinned public pack")
	}
	imports, err := config.LoadRootPackDefaultRigImports(fsys.OSFS{}, city.Dir)
	if err != nil {
		t.Fatal(err)
	}
	rolesPinned := false
	for _, item := range imports {
		if item.Import.Source == config.PublicGascityRolesPackSource && item.Import.Version == config.PublicGascityPackVersion {
			rolesPinned = true
		}
	}
	if !rolesPinned {
		t.Fatalf("factory lost its pinned default-rig imports: %+v", imports)
	}
	assertDoctorGreen(t, city, "ordinary split factory")
	city.RigAdd(rig, "")
	assertProxiedScope(t, env, rig, "factory rig work store")
	composed, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(city.Dir, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	rigAgents := 0
	for _, agent := range composed.Agents {
		if strings.HasPrefix(agent.QualifiedName(), filepath.Base(rig)+"/") {
			rigAgents++
		}
	}
	if rigAgents == 0 {
		t.Fatal("a normal rig add did not compose any factory rig agents")
	}
	out, err := city.GCStdout("bd", "create", "factory storage continuity", "--json")
	if err != nil {
		t.Fatalf("gc bd create: %v\n%s", err, out)
	}
	var bead struct {
		ID string `json:"id"`
	}
	lastJSONLine(t, out, &bead)
	if bead.ID == "" {
		t.Fatal("factory work bead has no identity")
	}
	stop := func() {
		t.Helper()
		if out, err := city.GC("stop", city.Dir); err != nil {
			t.Fatalf("gc stop: %v\n%s", err, out)
		}
		for _, root := range []string{city.Dir, rig} {
			if leaked := waitForNoDoltProcesses(t, root, 30*time.Second); len(leaked) != 0 {
				t.Fatalf("owned factory scopes survived stop:\n%s", strings.Join(leaked, "\n"))
			}
		}
	}
	stop()
	for _, args := range [][]string{{"doctor", "--check", "prompt-delivery-budget", "--json"}, {"doctor", "--json"}, {"doctor", "--fix", "--json"}} {
		out, runErr := city.GC(args...)
		var report doctorReport
		lastJSONLine(t, out, &report)
		gated := false
		for _, result := range report.Results {
			if result.Name == "prompt-delivery-budget" && strings.Contains(result.Message, doctor.StoreNotRunningMessage) {
				gated = true
			}
		}
		if !gated {
			t.Fatalf("%v did not report its stopped prompt-budget gate: %v\n%s", args, runErr, out)
		}
		for _, root := range []string{city.Dir, rig} {
			if started := helpers.WaitForDoltProcesses(t, root, 2*time.Second); len(started) != 0 {
				t.Fatalf("%v started stopped factory storage:\n%s\ndoctor report:\n%s", args, strings.Join(started, "\n"), out)
			}
		}
	}
	city.StartWithSupervisor()
	for _, args := range [][]string{{"bd", "show", bead.ID, "--json"}, {"bd", "list", "--json"}} {
		out, err := city.GCStdout(args...)
		if err != nil || !strings.Contains(out, bead.ID) {
			t.Fatalf("factory restart lost %s via %v: %v\n%s", bead.ID, args, err, out)
		}
	}
	stop()
}
