package contract

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// A city handed to bd by the journaled ownership handoff comes back published as
// `gc.endpoint_origin: city_canonical` over bd's own replacement server. Its
// rigs are untouched: the handoff is city-root only by design, and bd must not
// rewrite a scope it was not asked about.
//
// So the shape that now exists on disk is an `inherited_city` rig that tracks no
// endpoint of its own — which is what `inherited_city` has always meant — under
// a `city_canonical` city. gc used to require such a rig to carry a *mirror* of
// the city's host and port, and refused the whole city when it did not:
//
//	invalid canonical rig endpoint state in <rig>/.beads/config.yaml:
//	canonical inherited rig config requires both dolt.host and dolt.port
//
// That refusal is not scoped to the rig. It comes out of the canonical config
// resolver, so `gc start` and `gc doctor` fail for the entire city, and the
// operator has no command that fixes it — nothing wrote the rig config in the
// first place.
//
// Mirroring is the right rule for a rig that *does* track an endpoint: it must
// agree with its city. A rig that tracks none is inheriting, and following the
// city is both what the name says and what stays correct when the city's
// endpoint moves.

func writeConfigLines(t *testing.T, scopeRoot, body string) {
	t.Helper()
	fs := fsys.OSFS{}
	if err := fs.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(filepath.Join(scopeRoot, ".beads", "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// handedOffCityWithLegacyRig builds the post-handoff shape and returns both roots.
func handedOffCityWithLegacyRig(t *testing.T) (string, string) {
	t.Helper()
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "testrig")

	// What bd publishes over its replacement server.
	writeConfigLines(t, city, "issue_prefix: gc\n"+
		"gc.endpoint_origin: city_canonical\n"+
		"gc.endpoint_status: verified\n"+
		"dolt.host: 127.0.0.1\n"+
		"dolt.port: 34567\n")
	writeCanonicalMetadata(t, fs, city, "hq")

	// What the legacy gc wrote for the rig, and what bd correctly left alone.
	writeConfigLines(t, rig, "issue_prefix: ma\n"+
		"gc.endpoint_origin: inherited_city\n"+
		"gc.endpoint_status: verified\n")
	writeCanonicalMetadata(t, fs, rig, "ma")
	return city, rig
}

func TestInheritedRigUnderAHandedOffCityIsValid(t *testing.T) {
	city, rig := handedOffCityWithLegacyRig(t)
	fs := fsys.OSFS{}

	cfg, ok, err := ReadConfigState(fs, filepath.Join(rig, ".beads", "config.yaml"))
	if err != nil || !ok {
		t.Fatalf("read rig config: ok=%t err=%v", ok, err)
	}
	if err := ValidateCanonicalConfigState(fs, city, rig, cfg); err != nil {
		t.Fatalf("an inherited rig that tracks no endpoint was refused under a handed-off city: %v", err)
	}
}

func TestInheritedRigUnderAHandedOffCityResolvesThroughTheCityEndpoint(t *testing.T) {
	city, rig := handedOffCityWithLegacyRig(t)
	fs := fsys.OSFS{}

	target, err := ResolveDoltConnectionTarget(fs, city, rig)
	if err != nil {
		t.Fatalf("resolve the rig's connection under a handed-off city: %v", err)
	}
	if target.Host != "127.0.0.1" || target.Port != "34567" {
		t.Fatalf("rig target = %s:%s, want the city's published 127.0.0.1:34567", target.Host, target.Port)
	}
}

// The mirror rule still holds for a rig that does track an endpoint: if it
// claims one, it has to be the city's. Relaxing the no-endpoint case must not
// relax this one, or a rig can quietly point at a different server.
func TestInheritedRigThatTracksTheWrongEndpointIsStillRefused(t *testing.T) {
	city, rig := handedOffCityWithLegacyRig(t)
	fs := fsys.OSFS{}
	writeConfigLines(t, rig, "issue_prefix: ma\n"+
		"gc.endpoint_origin: inherited_city\n"+
		"dolt.host: 127.0.0.1\n"+
		"dolt.port: 19999\n")

	cfg, ok, err := ReadConfigState(fs, filepath.Join(rig, ".beads", "config.yaml"))
	if err != nil || !ok {
		t.Fatalf("read rig config: ok=%t err=%v", ok, err)
	}
	if err := ValidateCanonicalConfigState(fs, city, rig, cfg); err == nil {
		t.Fatal("an inherited rig pointing at a different port was accepted")
	}
}

// And a rig that carries half an endpoint is still wrong: that is a truncated
// mirror, not an inheritance.
func TestInheritedRigWithHalfAnEndpointIsStillRefused(t *testing.T) {
	city, rig := handedOffCityWithLegacyRig(t)
	fs := fsys.OSFS{}
	writeConfigLines(t, rig, "issue_prefix: ma\n"+
		"gc.endpoint_origin: inherited_city\n"+
		"dolt.host: 127.0.0.1\n")

	cfg, ok, err := ReadConfigState(fs, filepath.Join(rig, ".beads", "config.yaml"))
	if err != nil || !ok {
		t.Fatalf("read rig config: ok=%t err=%v", ok, err)
	}
	if err := ValidateCanonicalConfigState(fs, city, rig, cfg); err == nil {
		t.Fatal("an inherited rig carrying only a host was accepted")
	}
}

// The shape the ownership handoff actually leaves behind is a `managed_city`
// city whose gc runtime publication has been retired.
//
// The handoff transfers who RUNS the Dolt process, not who configured the
// endpoint, so `gc.endpoint_origin` stays `managed_city` — and the orchestrator
// removes `.gc/runtime/packs/dolt/dolt-state.json`, because leaving it says
// running:true for a pid that is gone. The city's own arm has always fallen
// through to bd's `.beads/dolt-server.pid`/`.port` when gc publishes no runtime
// state; the inherited-rig arm did not, and demanded the file:
//
//	read dolt runtime state: ... dolt-state.json: no such file or directory
//
// That refusal is the rig's, but its blast radius is the city's: `gc doctor`
// reports `rig:<name>:beads` as failed on a city that is working perfectly. A
// rig under a managed city inherits the city's endpoint; which process is
// serving it is not the rig's business, and the rig follows whichever record
// the city currently has.
func handedOffManagedCityWithLegacyRig(t *testing.T) (string, string) {
	t.Helper()
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "testrig")

	// What the legacy gc wrote, and what the handoff leaves alone: the city is
	// still a managed city by origin, because gc configured the endpoint.
	writeConfigLines(t, city, "issue_prefix: gc\n"+
		"gc.endpoint_origin: managed_city\n"+
		"gc.endpoint_status: verified\n")
	writeCanonicalMetadata(t, fs, city, "hq")
	writeConfigLines(t, rig, "issue_prefix: ma\n"+
		"gc.endpoint_origin: inherited_city\n"+
		"gc.endpoint_status: verified\n")
	writeCanonicalMetadata(t, fs, rig, "ma")
	return city, rig
}

func TestInheritedRigFollowsTheCityOntoBdsReplacementServer(t *testing.T) {
	city, rig := handedOffManagedCityWithLegacyRig(t)
	fs := fsys.OSFS{}
	// bd's commit: the city's own server record, and no gc publication.
	port := writeBdOwnedServerRecord(t, fs, city, os.Getpid())

	target, err := ResolveDoltConnectionTarget(fs, city, rig)
	if err != nil {
		t.Fatalf("resolve an inherited rig under a handed-off city: %v", err)
	}
	if target.Host != "127.0.0.1" || target.Port != port {
		t.Fatalf("rig target = %s:%s, want the city's replacement server 127.0.0.1:%s", target.Host, target.Port, port)
	}
	if target.Database != "ma" {
		t.Errorf("rig database = %q, want its own %q", target.Database, "ma")
	}
}

// And back again. A rollback republishes gc's runtime state and retires bd's
// record, and the rig has to follow that too — the rule is "whichever record
// the city currently has", not "prefer bd".
func TestInheritedRigFollowsTheCityBackOntoTheManagedServer(t *testing.T) {
	city, rig := handedOffManagedCityWithLegacyRig(t)
	fs := fsys.OSFS{}
	port := writeReachableRuntimeState(t, fs, city)

	target, err := ResolveDoltConnectionTarget(fs, city, rig)
	if err != nil {
		t.Fatalf("resolve an inherited rig under a restored managed city: %v", err)
	}
	if target.Port != port {
		t.Fatalf("rig target port = %s, want gc's republished %s", target.Port, port)
	}
}

// A managed city with neither record is still an error: nothing is serving the
// rig, and reporting a target would send every command at a dead endpoint.
func TestInheritedRigUnderACityWithNoServerStillFails(t *testing.T) {
	city, rig := handedOffManagedCityWithLegacyRig(t)
	fs := fsys.OSFS{}

	if _, err := ResolveDoltConnectionTarget(fs, city, rig); err == nil {
		t.Fatal("an inherited rig resolved under a city with no running server at all")
	} else if !IsManagedRuntimeUnavailable(err) {
		t.Fatalf("error = %v, want the managed-runtime-unavailable class", err)
	}
}
