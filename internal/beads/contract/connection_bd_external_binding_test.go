package contract

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// bd persists a direct scope's external upstream in its own metadata:
// dolt_server_host and dolt_server_port (or dolt_server_socket) are what
// `bd init --server --external --server-host --server-port` writes, and nothing
// else records that endpoint. gc leaves a provider-owned scope's config.yaml
// alone, so without reading that binding a direct-external city resolved as a
// managed city with no runtime state, and every ordinary command reported the
// store as down.

//nolint:unparam // helper keeps FS explicit for symmetry with related helpers
func writeRawMetadata(t *testing.T, fs fsys.FS, scopeRoot, raw string) {
	t.Helper()
	if err := fs.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(filepath.Join(scopeRoot, ".beads", "metadata.json"), []byte(raw+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

const bdExternalTCPMetadata = `{"database":"dolt","backend":"dolt","dolt_mode":"server",` +
	`"dolt_server_host":"db.example","dolt_server_port":4406,"dolt_database":"hosted"}`

func TestResolveDoltConnectionTargetReadsBdPersistedExternalBinding(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeBdTemplateConfig(t, fs, city, "gc")
	writeRawMetadata(t, fs, city, bdExternalTCPMetadata)

	target, err := ResolveDoltConnectionTarget(fs, city, city)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() on a bd-owned direct-external city: %v", err)
	}
	if !target.External || target.Host != "db.example" || target.Port != "4406" {
		t.Fatalf("target = %+v, want the external upstream db.example:4406", target)
	}
	if target.Database != "hosted" {
		t.Fatalf("target database = %q, want hosted", target.Database)
	}
}

func TestResolveDoltConnectionTargetReadsBdPersistedExternalSocketBinding(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeBdTemplateConfig(t, fs, city, "gc")
	writeRawMetadata(t, fs, city, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_server_socket":"/var/run/dolt.sock","dolt_database":"hosted"}`)

	target, err := ResolveDoltConnectionTarget(fs, city, city)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() on a socket-bound direct-external city: %v", err)
	}
	if !target.External || target.Socket != "/var/run/dolt.sock" {
		t.Fatalf("target = %+v, want the external socket upstream", target)
	}
}

// A rig inheriting from such a city has no binding of its own and must reach
// the same upstream rather than a managed server that does not exist.
func TestResolveDoltConnectionTargetInheritsBdPersistedExternalBinding(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "frontend")
	writeBdTemplateConfig(t, fs, city, "gc")
	writeRawMetadata(t, fs, city, bdExternalTCPMetadata)
	writeBdTemplateConfig(t, fs, rig, "fe")
	writeCanonicalMetadata(t, fs, rig, "fe")

	target, err := ResolveDoltConnectionTarget(fs, city, rig)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() on an inherited rig: %v", err)
	}
	if !target.External || target.Host != "db.example" || target.Port != "4406" {
		t.Fatalf("rig target = %+v, want the city's external upstream", target)
	}
}

// A canonical endpoint gc wrote stays authoritative: a persisted bd binding is
// the answer for a scope gc did not canonicalize, not an override of one it did.
func TestResolveDoltConnectionTargetKeepsCanonicalEndpointOverBdMetadataBinding(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginCityCanonical,
		EndpointStatus: EndpointStatusVerified,
		DoltHost:       "canonical.example",
		DoltPort:       "5506",
		DoltMode:       "server",
	})
	writeRawMetadata(t, fs, city, bdExternalTCPMetadata)

	target, err := ResolveDoltConnectionTarget(fs, city, city)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget(): %v", err)
	}
	if target.Host != "canonical.example" || target.Port != "5506" {
		t.Fatalf("target = %+v, want the canonical city endpoint", target)
	}
}

// A local server bd started still wins over a stale external marker: the pid
// record names a process that exists here and now.
func TestResolveDoltConnectionTargetPrefersLiveLocalServerOverExternalMarker(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeBdTemplateConfig(t, fs, city, "gc")
	writeRawMetadata(t, fs, city, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hosted"}`)
	port := writeBdOwnedServerRecord(t, fs, city, os.Getpid())

	target, err := ResolveDoltConnectionTarget(fs, city, city)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget(): %v", err)
	}
	if target.Port != port || target.External {
		t.Fatalf("target = %+v, want the live local server on %s", target, port)
	}
}
