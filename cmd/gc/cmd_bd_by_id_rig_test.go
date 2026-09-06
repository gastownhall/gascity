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

// writeRiggedForeignProviderCityTOML is writeForeignProviderCityTOML plus one
// bound rig, so the --rig flag has a real target to resolve to. Without a
// resolvable rig, resolveBdScopeTarget fails first with "rig not found" and the
// class-routing rule is never reached — which is a different, already-loud
// failure.
func writeRiggedForeignProviderCityTOML(t *testing.T, cityPath, rigPath string) {
	t.Helper()
	body := fmt.Sprintf(`[workspace]
name = "by-id-rig-city"

[[rigs]]
name = %q
path = %q
prefix = "wf"

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
`, byIDRigName, rigPath, config.StorageWorkBinding, string(configRefEngineProviderID))
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}
}

// riggedForeignProviderCity is foreignProviderCity with a bound rig, for the
// end-to-end doBd rows.
func riggedForeignProviderCity(t *testing.T) (cityPath string, classStore beads.Store) {
	t.Helper()
	clearGCEnv(t)
	cityPath = t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", byIDRigName)
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("creating the rig dir: %v", err)
	}
	writeRiggedForeignProviderCityTOML(t, cityPath, rigPath)
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
	return cityPath, store
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

// TestBdByIDRigRuleDoesNotBlameTheFlagForAnIDThatDoesNotExist pins the ORDER of
// the two refusals, which is the difference between a correct diagnosis and a
// wrong one.
//
// The --rig refusal asserts a fact: the binding owns this bead and the named
// rig's work store does not hold it. For a mistyped reserved-prefix id that
// sentence is false — nothing holds it — and the operator is sent to fix the
// flag, which was the one thing that was not wrong. The not-found answer has to
// come first.
func TestBdByIDRigRuleDoesNotBlameTheFlagForAnIDThatDoesNotExist(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	missing := reservedClassID(t, "9999")

	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, byIDRigName, []string{"show", missing}, &stdout, &stderr)
	if !handled || code != 1 {
		t.Fatalf("show %s under --rig exited %d (handled=%v), want a handled failure: %s", missing, code, handled, stderr.String())
	}
	msg := stderr.String()
	if !strings.Contains(msg, "no issue found") {
		t.Errorf("show %s under --rig reported %q, want the not-found answer — the id is what is wrong here, not the scope", missing, msg)
	}
	if strings.Contains(msg, "drop --rig") {
		t.Errorf("show %s under --rig blamed the flag: %q. The refusal claims the binding owns the bead and the rig store does not hold it; nothing holds it, so that claim is false", missing, msg)
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
	_, classStore := riggedForeignProviderCity(t)
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
