package contract

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// A bd-owned server-mode scope — what `gc init --beads-transport direct
// --beads-target local` produces — has no gc runtime state and never will. bd
// starts the Dolt process and records it in the scope's own
// .beads/dolt-server.pid and .port. Resolution has to read that record, or
// every ordinary command on the documented escape hatch resolves to "dolt
// runtime state unavailable" and `gc doctor` reports the store and the server
// as failed.
//
// gc's own managed lifecycle mirrors only the port, so the pid/port pair is
// what tells a live bd-owned server apart from a stale mirror left behind by a
// stopped managed city — the case the second test pins.

//nolint:unparam // helper keeps FS explicit for symmetry with related helpers
func writeBdOwnedServerRecord(t *testing.T, fs fsys.FS, scopeRoot string, pid int) string {
	t.Helper()
	port := listenReachablePort(t, "127.0.0.1")
	beadsDir := filepath.Join(scopeRoot, ".beads")
	if err := fs.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if pid > 0 {
		if err := fs.WriteFile(filepath.Join(beadsDir, "dolt-server.pid"), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := fs.WriteFile(filepath.Join(beadsDir, "dolt-server.port"), []byte(port+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return port
}

// writeBdTemplateConfig writes the .beads/config.yaml bd's own `init --server`
// leaves behind: a prefix and nothing gc wrote. gc deliberately does not
// canonicalize a provider-owned scope, so this is the real on-disk shape.
//
//nolint:unparam // helper keeps FS explicit for symmetry with related helpers
func writeBdTemplateConfig(t *testing.T, fs fsys.FS, scopeRoot, prefix string) {
	t.Helper()
	if err := fs.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(filepath.Join(scopeRoot, ".beads", "config.yaml"), []byte("issue_prefix: "+prefix+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveDoltConnectionTargetReadsBdOwnedServerRecord(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeBdTemplateConfig(t, fs, city, "gc")
	writeCanonicalMetadata(t, fs, city, "hq")
	port := writeBdOwnedServerRecord(t, fs, city, os.Getpid())

	target, err := ResolveDoltConnectionTarget(fs, city, city)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() on a bd-owned direct city: %v", err)
	}
	if target.Port != port || target.Host != "127.0.0.1" {
		t.Fatalf("target = %+v, want 127.0.0.1:%s from bd's own server record", target, port)
	}
	if target.External {
		t.Fatalf("target = %+v, want a local server, not an external endpoint", target)
	}
}

// A rig in a bd-owned direct city runs its own server; it must resolve to that
// one, not to whatever the city's server happens to be listening on.
func TestResolveDoltConnectionTargetReadsBdOwnedRigServerRecord(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "frontend")
	writeBdTemplateConfig(t, fs, city, "gc")
	writeCanonicalMetadata(t, fs, city, "hq")
	cityPort := writeBdOwnedServerRecord(t, fs, city, os.Getpid())

	writeBdTemplateConfig(t, fs, rig, "fe")
	writeCanonicalMetadata(t, fs, rig, "fe")
	rigPort := writeBdOwnedServerRecord(t, fs, rig, os.Getpid())
	if rigPort == cityPort {
		t.Fatalf("fixture gave the rig the city's port %s", cityPort)
	}

	target, err := ResolveDoltConnectionTarget(fs, city, rig)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() on a bd-owned direct rig: %v", err)
	}
	if target.Port != rigPort {
		t.Fatalf("rig target = %+v, want its own server on port %s", target, rigPort)
	}
}

// A stale port mirror is not a server. gc's managed lifecycle writes
// .beads/dolt-server.port and no pid file, so a stopped managed city must keep
// reporting its runtime state as unavailable rather than resolving to a port
// nothing is listening on.
func TestResolveDoltConnectionTargetIgnoresPortMirrorWithoutBdPID(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	})
	writeCanonicalMetadata(t, fs, city, "hq")
	writeBdOwnedServerRecord(t, fs, city, 0)

	if _, err := ResolveDoltConnectionTarget(fs, city, city); err == nil || !IsManagedRuntimeUnavailable(err) {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v, want managed runtime unavailable", err)
	}
}

// A pid file naming a process that is gone is the crash case: bd's record
// outlived its server. It must not be mistaken for a live binding.
func TestResolveDoltConnectionTargetIgnoresDeadBdServerRecord(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	})
	writeCanonicalMetadata(t, fs, city, "hq")
	writeBdOwnedServerRecord(t, fs, city, 999999)

	if _, err := ResolveDoltConnectionTarget(fs, city, city); err == nil || !IsManagedRuntimeUnavailable(err) {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v, want managed runtime unavailable", err)
	}
}

// gc's own runtime state still wins where it exists: a managed city whose
// server gc started must keep resolving through it, so a leftover bd record
// cannot redirect a scope gc owns.
func TestResolveDoltConnectionTargetPrefersGcRuntimeStateOverBdRecord(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	})
	writeCanonicalMetadata(t, fs, city, "hq")
	managedPort := writeReachableRuntimeState(t, fs, city)
	bdPort := writeBdOwnedServerRecord(t, fs, city, os.Getpid())
	if managedPort == bdPort {
		t.Fatalf("fixture reused port %s for both records", managedPort)
	}

	target, err := ResolveDoltConnectionTarget(fs, city, city)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget(): %v", err)
	}
	if target.Port != managedPort {
		t.Fatalf("target = %+v, want gc's own managed port %s", target, managedPort)
	}
}

func TestResolveDoltConnectionTargetKeepsExternalEndpointOverBdRecord(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginCityCanonical,
		EndpointStatus: EndpointStatusVerified,
		DoltHost:       "127.0.0.1",
		DoltPort:       "7777",
		DoltMode:       "server",
	})
	writeCanonicalMetadata(t, fs, city, "hq")
	writeBdOwnedServerRecord(t, fs, city, os.Getpid())

	target, err := ResolveDoltConnectionTarget(fs, city, city)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget(): %v", err)
	}
	if !target.External || target.Port != "7777" {
		t.Fatalf("target = %+v, want the canonical external endpoint 127.0.0.1:7777", target)
	}
}

func TestResolveDoltConnectionTargetRejectsUnparseableBdServerRecord(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	})
	writeCanonicalMetadata(t, fs, city, "hq")
	beadsDir := filepath.Join(city, ".beads")
	if err := fs.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(filepath.Join(beadsDir, "dolt-server.pid"), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(filepath.Join(beadsDir, "dolt-server.port"), []byte("not-a-port\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := ResolveDoltConnectionTarget(fs, city, city)
	if err == nil || !strings.Contains(err.Error(), "dolt runtime state unavailable") {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v, want managed runtime unavailable", err)
	}
}
