package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// This file pins the --rig RULE for `gc bd`'s by-id surface.
//
// The rule: an EXPLICIT --rig naming a work rig scope, combined with a bead the
// relocated class binding owns, is refused. Neither of the silent answers is
// taken — serving it ignores a flag the operator reached for to be MORE
// specific, and honoring it routes the read at a ledger that does not hold the
// bead and answers an empty result indistinguishable from a real one.
//
// Everything else is untouched, and each carve-out has a row below: a work id
// under --rig still passes through, a class id with no --rig is still served,
// auto-detected scope (GC_RIG) is not a deliberate scope and does not refuse,
// and a city that relocates nothing never reaches the rule at all.

const byIDRigName = "workflows"

// byIDRigWorkPrefix is the ordinary, non-shadowing rig prefix. Rows that care
// about the reserved namespace pass their own.
const byIDRigWorkPrefix = "wf"

// writeRiggedForeignProviderCityTOML is writeForeignProviderCityTOML plus one
// bound rig, so the --rig flag has a real target to resolve to. Without a
// resolvable rig, resolveBdScopeTarget fails first with "rig not found" and the
// class-routing rule is never reached — which is a different, already-loud
// failure.
//
// The rig's prefix is a parameter because it is the input to the shadow rule:
// config.ValidateRigs does not reject a prefix inside a relocated class's
// reserved namespace, so a real city can carry one and the door has to answer
// for it.
func writeRiggedForeignProviderCityTOML(t *testing.T, cityPath, rigPath, rigPrefix string) {
	t.Helper()
	body := fmt.Sprintf(`[workspace]
name = "by-id-rig-city"

[[rigs]]
name = %q
path = %q
prefix = %q

[storage.classes]
work = %q
graph = "infra"
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "infra"

[storage.bindings.infra]
provider = %q
config_ref = "infra"
`, byIDRigName, rigPath, rigPrefix, config.StorageWorkBinding, string(configRefEngineProviderID))
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}
}

// riggedForeignProviderCity is foreignProviderCity with a bound rig, for the
// end-to-end doBd rows. It returns the rig's root beside the city's so a caller
// can seed the rig's own work ledger.
func riggedForeignProviderCity(t *testing.T, rigPrefix string) (cityPath, rigPath string, classStore beads.Store) {
	t.Helper()
	clearGCEnv(t)
	cityPath = t.TempDir()
	rigPath = filepath.Join(cityPath, "rigs", byIDRigName)
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("creating the rig dir: %v", err)
	}
	writeRiggedForeignProviderCityTOML(t, cityPath, rigPath, rigPrefix)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_CITY", cityPath)
	registerConfigRefEngineProvider(t)
	stubInfraMigrationSource(t)
	resetCLIStorageRoutes(t)
	captureCLIStorageStderr(t)

	store, relocated := graphClassBinding(cliStorageRoutes(cityPath))
	if !relocated {
		t.Fatal("a city serving its classes from a foreign provider resolved no class binding")
	}
	return cityPath, rigPath, store
}

// seedRigWorkBead plants a bead in the rig's OWN file-backed work ledger, under
// a pinned id in the rig's configured namespace, and returns it.
//
// It opens the file the rig scope resolves to directly rather than through
// openStoreAtForCity, because the id has to be pinned: a minted one would race
// the binding's own sequence, which mints from the same reserved prefix when the
// rig shadows it, and a fixture whose two stores can agree on an id proves
// nothing about which one answered. The caller re-reads it through the
// production accessor, so the seam this bypasses is still covered.
func seedRigWorkBead(t *testing.T, cityPath, rigPath, rigPrefix, title string) beads.Bead {
	t.Helper()
	beadsPath := filepath.Join(rigPath, ".gc", "beads.json")
	if err := os.MkdirAll(filepath.Dir(beadsPath), 0o755); err != nil {
		t.Fatalf("creating the rig's store dir: %v", err)
	}
	store, err := beads.OpenFileStore(fsys.OSFS{}, beadsPath)
	if err != nil {
		t.Fatalf("opening the rig's work ledger at %s: %v", beadsPath, err)
	}
	store.IDPrefix = rigPrefix
	store.HonorExplicitIDs = true
	created, err := store.Create(beads.Bead{ID: rigPrefix + "-rig1", Title: title, Type: "task"})
	if err != nil {
		t.Fatalf("seeding the rig's work ledger: %v", err)
	}

	// Re-read through the accessor production points the passthrough's scope at,
	// so the row is proved reachable on the far side of the door rather than only
	// present in a file.
	viaProduction, err := openStoreAtForCity(rigPath, cityPath)
	if err != nil {
		t.Fatalf("opening the rig scope the way production does: %v", err)
	}
	if _, err := viaProduction.Get(created.ID); err != nil {
		t.Fatalf("the rig scope does not resolve %s: %v — the seed did not land where the passthrough would read it", created.ID, err)
	}
	return created
}

// TestBdByIDRefusesAnExplicitRigScopeOnAClassOwnedBead is the rule.
func TestBdByIDRefusesAnExplicitRigScopeOnAClassOwnedBead(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	bead := mustCreateClassBead(t, classStore, beads.Bead{Title: "owned by the binding", Type: "task"})

	for _, args := range [][]string{
		{"show", bead.ID},
		{"show", bead.ID, "--json"},
		{"update", bead.ID, "--claim"},
		{"update", bead.ID, "--set-metadata", "gc.outcome=pass", "--status", "closed"},
		{"dep", "list", bead.ID},
		{"release-if-current", bead.ID, "someone"},
	} {
		var stdout, stderr bytes.Buffer
		code, handled := maybeRouteBdByID(cityPath, byIDRigName, args, &stdout, &stderr)
		if !handled {
			t.Fatalf("%v under --rig fell through to the bd subprocess, which would run it against the rig's work store", args)
		}
		if code != 1 {
			t.Errorf("%v under --rig exited %d, want 1", args, code)
		}
		if stdout.Len() != 0 {
			t.Errorf("%v under --rig wrote %q to stdout; a refused command must serve nothing", args, stdout.String())
		}
		msg := stderr.String()
		for _, want := range []string{bead.ID, "--rig " + byIDRigName, "not partitioned by rig", "drop --rig"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%v under --rig: refusal %q does not name %q", args, msg, want)
			}
		}
	}

	// The mutation guard: with no --rig the same invocations are SERVED. If this
	// row ever fails alongside the rows above, the rule has become a blanket
	// refusal rather than a scope-coherence rule.
	var stdout, stderr bytes.Buffer
	if code, handled := maybeRouteBdByID(cityPath, "", []string{"show", bead.ID, "--json"}, &stdout, &stderr); !handled || code != 0 {
		t.Fatalf("show without --rig exited %d (handled=%v): %s", code, handled, stderr.String())
	}
	if !strings.Contains(stdout.String(), bead.ID) {
		t.Errorf("show without --rig printed %q, want the bead", stdout.String())
	}
}

// TestBdByIDReservedMissUnderExplicitRigFallsThrough pins the ORDER of the two
// arms, which is the difference between a correct diagnosis and a wrong one.
//
// The --rig refusal asserts a fact: the binding owns this bead and the named
// rig's work store does not hold it. For a reserved-prefix id the binding does
// not hold, that sentence is false, and blaming the flag sends the operator to
// fix the one thing that was not wrong. The residence answer comes first — and
// since the binding is only the namespace's AUTHORITY, its miss hands the id to
// the passthrough, which already carries the rig scope the operator asked for.
// That is exactly the population --rig is for: a rig legitimately configured
// with a prefix inside the reserved namespace.
func TestBdByIDReservedMissUnderExplicitRigFallsThrough(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	missing := reservedClassID(t, "9999")

	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, byIDRigName, []string{"show", missing}, &stdout, &stderr)
	if handled {
		t.Fatalf("show %s under --rig was answered here (exit %d): %s%s — the binding does not hold it, so the rig's own store is what the flag pinned", missing, code, stdout.String(), stderr.String())
	}

	// The control: the same city, the same flag, an id the binding DOES own
	// still refuses. Without this row the fix could be "stop refusing".
	bead := mustCreateClassBead(t, classStore, beads.Bead{Title: "owned by the binding", Type: "task"})
	stdout.Reset()
	stderr.Reset()
	if code, handled := maybeRouteBdByID(cityPath, byIDRigName, []string{"show", bead.ID}, &stdout, &stderr); !handled || code != 1 {
		t.Fatalf("show %s under --rig exited %d (handled=%v), want the refusal", bead.ID, code, handled)
	}
	if !strings.Contains(stderr.String(), "drop --rig") {
		t.Errorf("show %s under --rig lost the refusal: %q", bead.ID, stderr.String())
	}
}

// TestBdByIDShadowPrefixRigBeadReachesThePassthrough is the population proof
// behind the fall-through: the beads a binding miss is ABOUT, and where they
// actually live.
//
// A rig may lawfully be configured with a prefix inside a relocated class's
// reserved namespace. config.ValidateRigs does not reject one and
// config.ReservedPrefixWarnings only advises, so such a rig starts, serves, and
// mints work beads carrying ids the binding has never held and never will. On a
// split city every `gc bd` invocation naming one used to be answered at this
// door — a read reported as absent and a write refused — including the
// step-completion write the core pack renders on every worked bead. The binding
// is the namespace's AUTHORITY, not its only lawful holder, so its miss hands
// the id to the passthrough that is pointed at the ledger holding it.
//
// Both spellings are driven. The exact reserved prefix is the one an operator
// gets warned about; the longer prefix inside the same namespace is the one
// IsReservedClassPrefix's exact match does not warn about at all, and is
// therefore the likelier of the two to be running somewhere.
func TestBdByIDShadowPrefixRigBeadReachesThePassthrough(t *testing.T) {
	reserved, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok || reserved == "" {
		t.Fatalf("no reserved id prefix is registered for the %q class", config.BeadClassGraph)
	}
	for _, rigPrefix := range []string{reserved, reserved + "-alpha"} {
		t.Run(rigPrefix, func(t *testing.T) {
			cityPath, rigPath, classStore := riggedForeignProviderCity(t, rigPrefix)
			resident := seedRigWorkBead(t, cityPath, rigPath, rigPrefix, "a rig work bead inside the reserved namespace")
			if !bdIDIsClassReserved(resident.ID) {
				t.Fatalf("the rig's id %q is outside the reserved namespace; the fixture proves nothing about the shadow rule", resident.ID)
			}
			if _, err := classStore.Get(resident.ID); err == nil {
				t.Fatalf("the class binding also holds %s; the fixture cannot tell a fall-through from a served read", resident.ID)
			}

			for _, rig := range []string{"", byIDRigName} {
				var stdout, stderr bytes.Buffer
				code, handled := maybeRouteBdByID(cityPath, rig, []string{"show", resident.ID}, &stdout, &stderr)
				if handled {
					t.Fatalf("show %s (--rig %q) was answered at this door (exit %d): %s%s — the only ledger holding it is the rig's, on the far side of the passthrough", resident.ID, rig, code, stdout.String(), stderr.String())
				}
			}

			// The control: on the same city an id the binding DOES hold is still
			// served here. Without it the fix could be "stop routing".
			held := mustCreateClassBead(t, classStore, beads.Bead{Title: "held by the binding", Type: "task"})
			if held.ID == resident.ID {
				t.Fatalf("the binding minted the rig's id %s; the two ledgers agreed and the control is meaningless", held.ID)
			}
			var stdout, stderr bytes.Buffer
			if code, handled := maybeRouteBdByID(cityPath, "", []string{"show", held.ID, "--json"}, &stdout, &stderr); !handled || code != 0 {
				t.Fatalf("show %s = (%d, %t): %s", held.ID, code, handled, stderr.String())
			}
			if !strings.Contains(stdout.String(), held.ID) {
				t.Errorf("the served show printed %q, want %s", stdout.String(), held.ID)
			}
		})
	}
}

// TestBdByIDRigScopeLeavesWorkStoreIDsToThePassthrough is the carve-out that
// keeps the rule from becoming "any --rig is refused on a split city". A rig id
// under --rig is exactly what the flag is for, the class store never held it,
// and it must reach bd unchanged.
func TestBdByIDRigScopeLeavesWorkStoreIDsToThePassthrough(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)
	var stdout, stderr bytes.Buffer
	if code, handled := maybeRouteBdByID(cityPath, byIDRigName, []string{"show", "wf-abc123"}, &stdout, &stderr); handled {
		t.Fatalf("a work-store id under --rig was handled in process (code %d): %s", code, stderr.String())
	}
}

// TestBdByIDRigScopeIsInertOnACityThatRelocatesNothing is the single-store
// compatibility claim for the rule: a legacy city mints no class-owned ids and
// opens no binding, so --rig cannot refuse anything.
func TestBdByIDRigScopeIsInertOnACityThatRelocatesNothing(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"legacy\"\n"), 0o644); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_CITY", cityPath)
	resetCLIStorageRoutes(t)
	captureCLIStorageStderr(t)

	for _, id := range []string{"gc-abc123", reservedClassID(t, "anything")} {
		var stdout, stderr bytes.Buffer
		if code, handled := maybeRouteBdByID(cityPath, byIDRigName, []string{"show", id}, &stdout, &stderr); handled {
			t.Fatalf("show %s under --rig was handled on an unrelocated city (code %d): %s", id, code, stderr.String())
		}
	}
}

// TestBdByIDRigRuleAppliesToTheFlagAndNotToAutoDetectedScope is the carve-out
// that matters most in production. The controller sets GC_RIG on every rig
// agent, so a rule that read auto-detected scope as a deliberate one would
// refuse the step-completion write the core pack renders on every worked bead.
//
// It drives the whole of doBd, because GC_RIG is resolved inside
// resolveBdScopeTarget and never reaches the by-id surface — a unit call could
// not tell the two apart.
func TestBdByIDRigRuleAppliesToTheFlagAndNotToAutoDetectedScope(t *testing.T) {
	_, _, classStore := riggedForeignProviderCity(t, byIDRigWorkPrefix)
	bead := mustCreateClassBead(t, classStore, beads.Bead{Title: "a worked step", Type: "task"})

	t.Setenv("GC_RIG", byIDRigName)
	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"show", bead.ID, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("GC_RIG=%s show exited %d: %s", byIDRigName, code, stderr.String())
	}
	if !strings.Contains(stdout.String(), bead.ID) {
		t.Errorf("GC_RIG=%s show printed %q, want the bead served from the binding", byIDRigName, stdout.String())
	}

	// The same city, the same bead, the same rig — written as the flag.
	stdout.Reset()
	stderr.Reset()
	if code := doBd([]string{"--rig", byIDRigName, "show", bead.ID, "--json"}, &stdout, &stderr); code != 1 {
		t.Fatalf("--rig %s show exited %d, want 1: stdout=%q stderr=%q", byIDRigName, code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "drop --rig") {
		t.Errorf("--rig %s show: stderr %q does not carry the refusal", byIDRigName, stderr.String())
	}

	// `gc --rig X bd ...` is the same explicit flag by another spelling, and
	// extractBdScopeFlags folds it into the same value.
	stdout.Reset()
	stderr.Reset()
	prev := rigFlag
	rigFlag = byIDRigName
	t.Cleanup(func() { rigFlag = prev })
	if code := doBd([]string{"show", bead.ID, "--json"}, &stdout, &stderr); code != 1 {
		t.Fatalf("gc --rig %s bd show exited %d, want 1: stdout=%q stderr=%q", byIDRigName, code, stdout.String(), stderr.String())
	}
}
