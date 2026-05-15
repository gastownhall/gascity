package contract

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestMain(m *testing.M) {
	_ = os.Unsetenv(ManagedCityHostEnv)
	os.Exit(m.Run())
}

func TestResolveDoltConnectionTargetManagedCity(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	})
	writeCanonicalMetadata(t, fs, city, "hq")
	port := writeReachableRuntimeState(t, fs, city)

	target, err := ResolveDoltConnectionTarget(fs, city, city)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v", err)
	}
	if target.External {
		t.Fatal("managed city target should not be external")
	}
	if target.Host != "127.0.0.1" || target.Port != port || target.Database != "hq" {
		t.Fatalf("target = %+v", target)
	}
}

func TestResolveDoltConnectionTargetLegacyManagedCity(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{IssuePrefix: "gc"})
	writeCanonicalMetadata(t, fs, city, "hq")
	port := writeReachableRuntimeState(t, fs, city)

	target, err := ResolveDoltConnectionTarget(fs, city, city)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v", err)
	}
	if target.EndpointOrigin != EndpointOriginManagedCity || target.EndpointStatus != EndpointStatusVerified {
		t.Fatalf("legacy managed city derived %+v", target)
	}
	if target.External || target.Host != "127.0.0.1" || target.Port != port {
		t.Fatalf("target = %+v", target)
	}
}

func TestResolveDoltConnectionTargetLegacyExternalCity(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{IssuePrefix: "gc", DoltHost: "db.example.com", DoltPort: "4406"})
	writeCanonicalMetadata(t, fs, city, "hq")

	target, err := ResolveDoltConnectionTarget(fs, city, city)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v", err)
	}
	if target.EndpointOrigin != EndpointOriginCityCanonical || target.EndpointStatus != EndpointStatusUnverified {
		t.Fatalf("legacy external city derived %+v", target)
	}
	if !target.External || target.Host != "db.example.com" || target.Port != "4406" {
		t.Fatalf("target = %+v", target)
	}
}

func TestResolveDoltConnectionTargetInheritedExternalRig(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginCityCanonical,
		EndpointStatus: EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       "4406",
		DoltUser:       "city-user",
	})
	writeCanonicalConfig(t, fs, rig, ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: EndpointOriginInheritedCity,
		EndpointStatus: EndpointStatusUnverified,
		DoltHost:       "db.example.com",
		DoltPort:       "4406",
		DoltUser:       "city-user",
	})
	writeCanonicalMetadata(t, fs, rig, "fe")

	target, err := ResolveDoltConnectionTarget(fs, city, rig)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v", err)
	}
	if !target.External {
		t.Fatal("inherited external rig should resolve external target")
	}
	if target.Host != "db.example.com" || target.Port != "4406" || target.Database != "fe" {
		t.Fatalf("target = %+v", target)
	}
	if target.User != "city-user" {
		t.Fatalf("target.User = %q, want city canonical user", target.User)
	}
	if target.EndpointStatus != EndpointStatusVerified {
		t.Fatalf("target.EndpointStatus = %q, want %q", target.EndpointStatus, EndpointStatusVerified)
	}
}

func TestResolveDoltConnectionTargetRejectsInheritedExternalRigEndpointMismatch(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginCityCanonical,
		EndpointStatus: EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       "4406",
		DoltUser:       "city-user",
	})
	writeCanonicalConfig(t, fs, rig, ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: EndpointOriginInheritedCity,
		EndpointStatus: EndpointStatusUnverified,
		DoltHost:       "other.example.com",
		DoltPort:       "5507",
		DoltUser:       "city-user",
	})
	writeCanonicalMetadata(t, fs, rig, "fe")

	if _, err := ResolveDoltConnectionTarget(fs, city, rig); err == nil || !strings.Contains(err.Error(), "mirror the city endpoint") {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v, want inherited endpoint mismatch rejection", err)
	}
}

func TestResolveDoltConnectionTargetTreatsSymlinkedCityAsCityScope(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	cityLink := filepath.Join(t.TempDir(), "city-link")
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	})
	writeCanonicalMetadata(t, fs, city, "hq")
	port := writeReachableRuntimeState(t, fs, city)
	if err := os.Symlink(city, cityLink); err != nil {
		t.Fatal(err)
	}

	target, err := ResolveDoltConnectionTarget(fs, city, cityLink)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v", err)
	}
	if target.External {
		t.Fatal("symlinked city target should remain managed")
	}
	if target.Host != "127.0.0.1" || target.Port != port || target.Database != "hq" {
		t.Fatalf("target = %+v", target)
	}
}

func TestResolveAuthoritativeConfigStateDerivesLegacyManagedRigFromCityRuntime(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	})
	writeCanonicalConfig(t, fs, rig, ConfigState{
		IssuePrefix: "fe",
		DoltPort:    "5507",
	})
	writeCanonicalMetadata(t, fs, rig, "fe")
	_ = writeReachableRuntimeState(t, fs, city)

	state, ok, err := ResolveAuthoritativeConfigState(fs, city, rig, "fe")
	if err != nil {
		t.Fatalf("ResolveAuthoritativeConfigState() error = %v", err)
	}
	if !ok {
		t.Fatal("ResolveAuthoritativeConfigState() = not authoritative, want inherited managed state")
	}
	if state.EndpointOrigin != EndpointOriginInheritedCity || state.EndpointStatus != EndpointStatusVerified {
		t.Fatalf("state = %+v", state)
	}
	if state.DoltHost != "" || state.DoltPort != "" || state.DoltUser != "" {
		t.Fatalf("state = %+v", state)
	}
}

func TestResolveDoltConnectionTargetRejectsInheritedRigWhenCityConfigIsInvalid(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
		DoltHost:       "stale.example.com",
		DoltPort:       "4406",
	})
	writeCanonicalConfig(t, fs, rig, ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: EndpointOriginInheritedCity,
		EndpointStatus: EndpointStatusVerified,
	})
	writeCanonicalMetadata(t, fs, rig, "fe")
	_ = writeReachableRuntimeState(t, fs, city)

	if _, err := ResolveDoltConnectionTarget(fs, city, rig); err == nil || !strings.Contains(err.Error(), "must not track") {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v, want invalid parent city rejection", err)
	}
}

func TestResolveDoltConnectionTargetLegacyInheritedExternalRig(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")
	writeCanonicalConfig(t, fs, city, ConfigState{IssuePrefix: "gc", DoltHost: "db.example.com", DoltPort: "4406"})
	writeCanonicalConfig(t, fs, rig, ConfigState{IssuePrefix: "fe", DoltHost: "db.example.com", DoltPort: "4406"})
	writeCanonicalMetadata(t, fs, rig, "fe")

	target, err := ResolveDoltConnectionTarget(fs, city, rig)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v", err)
	}
	if target.EndpointOrigin != EndpointOriginInheritedCity || target.EndpointStatus != EndpointStatusUnverified {
		t.Fatalf("legacy inherited external rig derived %+v", target)
	}
	if !target.External || target.Host != "db.example.com" || target.Port != "4406" {
		t.Fatalf("target = %+v", target)
	}
}

func TestResolveDoltConnectionTargetLegacyPortOnlyRigUnderManagedCityStaysInherited(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")
	writeCanonicalConfig(t, fs, rig, ConfigState{IssuePrefix: "fe", DoltPort: "5507"})
	writeCanonicalMetadata(t, fs, rig, "fe")
	port := writeReachableRuntimeState(t, fs, city)

	target, err := ResolveDoltConnectionTarget(fs, city, rig)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v", err)
	}
	if target.EndpointOrigin != EndpointOriginInheritedCity || target.EndpointStatus != EndpointStatusVerified {
		t.Fatalf("target = %+v", target)
	}
	if target.External || target.Host != "127.0.0.1" || target.Port != port || target.Database != "fe" {
		t.Fatalf("target = %+v", target)
	}
}

func TestResolveDoltConnectionTargetLegacyExplicitExternalRig(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")
	writeCanonicalConfig(t, fs, city, ConfigState{IssuePrefix: "gc", DoltHost: "db.example.com", DoltPort: "4406"})
	writeCanonicalConfig(t, fs, rig, ConfigState{IssuePrefix: "fe", DoltHost: "other.example.com", DoltPort: "5507"})
	writeCanonicalMetadata(t, fs, rig, "fe")

	target, err := ResolveDoltConnectionTarget(fs, city, rig)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v", err)
	}
	if target.EndpointOrigin != EndpointOriginExplicit || target.EndpointStatus != EndpointStatusUnverified {
		t.Fatalf("legacy explicit rig derived %+v", target)
	}
	if !target.External || target.Host != "other.example.com" || target.Port != "5507" {
		t.Fatalf("target = %+v", target)
	}
}

func TestResolveDoltConnectionTargetInheritedManagedRigUsesCityRuntime(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")
	writeCanonicalConfig(t, fs, rig, ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: EndpointOriginInheritedCity,
		EndpointStatus: EndpointStatusVerified,
	})
	writeCanonicalMetadata(t, fs, rig, "fe")
	port := writeReachableRuntimeState(t, fs, city)

	target, err := ResolveDoltConnectionTarget(fs, city, rig)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v", err)
	}
	if target.External {
		t.Fatal("inherited managed rig should not resolve external target")
	}
	if target.Host != "127.0.0.1" || target.Port != port || target.Database != "fe" {
		t.Fatalf("target = %+v", target)
	}
}

func TestResolveDoltConnectionTargetInheritedManagedRig_EnvOverride(t *testing.T) {
	host := reachableNonLoopbackHost(t)
	t.Setenv(ManagedCityHostEnv, host)
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	})
	writeCanonicalConfig(t, fs, rig, ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: EndpointOriginInheritedCity,
		EndpointStatus: EndpointStatusVerified,
	})
	writeCanonicalMetadata(t, fs, rig, "fe")
	port := writeReachableRuntimeStateOnHost(t, fs, city, host)

	target, err := ResolveDoltConnectionTarget(fs, city, rig)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v", err)
	}
	if target.External {
		t.Fatal("inherited managed rig should not resolve external target")
	}
	if target.Host != host || target.Port != port || target.Database != "fe" {
		t.Fatalf("target = %+v", target)
	}
}

func TestResolveDoltConnectionTargetLegacyInheritedManagedRigUsesCityRuntime(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")
	writeCanonicalConfig(t, fs, rig, ConfigState{IssuePrefix: "fe"})
	writeCanonicalMetadata(t, fs, rig, "fe")
	port := writeReachableRuntimeState(t, fs, city)

	target, err := ResolveDoltConnectionTarget(fs, city, rig)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v", err)
	}
	if target.EndpointOrigin != EndpointOriginInheritedCity || target.EndpointStatus != EndpointStatusVerified {
		t.Fatalf("legacy inherited managed rig derived %+v", target)
	}
	if target.External || target.Host != "127.0.0.1" || target.Port != port {
		t.Fatalf("target = %+v", target)
	}
}

func TestResolveDoltConnectionTargetRequiresRuntimeForManagedScopes(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	})
	writeCanonicalMetadata(t, fs, city, "hq")

	if _, err := ResolveDoltConnectionTarget(fs, city, city); err == nil {
		t.Fatal("ResolveDoltConnectionTarget() should fail without runtime state")
	}
}

func TestResolveDoltConnectionTargetRejectsManagedRuntimeStateWithUnreachablePort(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	})
	writeCanonicalMetadata(t, fs, city, "hq")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	writeRuntimeState(t, fs, city, fmt.Sprintf(`{"running":true,"pid":%d,"port":%d,"data_dir":%q}`, os.Getpid(), port, filepath.Join(city, ".beads", "dolt")))

	if _, err := ResolveDoltConnectionTarget(fs, city, city); err == nil || !strings.Contains(err.Error(), "dolt runtime state unavailable") {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v, want unreachable managed runtime rejection", err)
	}
}

func TestResolveDoltConnectionTargetRejectsManagedRuntimeStateWithWrongDataDir(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	})
	writeCanonicalMetadata(t, fs, city, "hq")
	port := writeReachableRuntimeStateWithDataDir(t, fs, city, filepath.Join(t.TempDir(), "other-dolt"))
	if port == "" {
		t.Fatal("expected reachable port")
	}

	if _, err := ResolveDoltConnectionTarget(fs, city, city); err == nil || !strings.Contains(err.Error(), "dolt runtime state unavailable") {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v, want wrong data dir rejection", err)
	}
}

func TestResolveDoltConnectionTargetRejectsManagedRuntimeStateWithDeadPID(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	})
	writeCanonicalMetadata(t, fs, city, "hq")
	writeRuntimeState(t, fs, city, `{"running":true,"pid":999999,"port":3307}`)

	if _, err := ResolveDoltConnectionTarget(fs, city, city); err == nil || !strings.Contains(err.Error(), "dolt runtime state unavailable") {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v, want stale managed runtime rejection", err)
	}
}

func TestResolveDoltConnectionTargetManagedCity_EnvOverrideSkipsLocalPID(t *testing.T) {
	host := reachableNonLoopbackHost(t)
	t.Setenv(ManagedCityHostEnv, host)
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	})
	writeCanonicalMetadata(t, fs, city, "hq")
	port := writeReachableRuntimeStateOnHostWithPID(t, fs, city, host, 99999999)

	target, err := ResolveDoltConnectionTarget(fs, city, city)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v", err)
	}
	if target.Host != host || target.Port != port || target.Database != "hq" {
		t.Fatalf("target = %+v", target)
	}
}

func TestResolveDoltConnectionTargetManagedCity_LoopbackAliasRequiresLocalPID(t *testing.T) {
	t.Setenv(ManagedCityHostEnv, "127.0.0.2")
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	})
	writeCanonicalMetadata(t, fs, city, "hq")
	writeRuntimeState(t, fs, city, fmt.Sprintf(`{"running":true,"pid":99999999,"port":3307,"data_dir":%q}`, filepath.Join(city, ".beads", "dolt")))

	if _, err := ResolveDoltConnectionTarget(fs, city, city); err == nil || !strings.Contains(err.Error(), "dolt runtime state unavailable") {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v, want stale loopback runtime rejection", err)
	}
}

func TestResolveDoltConnectionTargetRejectsManagedRuntimeStateWithZombiePID(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("zombie detection uses /proc on linux")
	}
	proc := exec.Command("sh", "-c", "exit 0")
	if err := proc.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = proc.Wait() }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		fs := fsys.OSFS{}
		city := t.TempDir()
		writeCanonicalConfig(t, fs, city, ConfigState{
			IssuePrefix:    "gc",
			EndpointOrigin: EndpointOriginManagedCity,
			EndpointStatus: EndpointStatusVerified,
		})
		writeCanonicalMetadata(t, fs, city, "hq")
		writeRuntimeState(t, fs, city, fmt.Sprintf(`{"running":true,"pid":%d,"port":3307}`, proc.Process.Pid))
		_, err := ResolveDoltConnectionTarget(fs, city, city)
		if err != nil && strings.Contains(err.Error(), "dolt runtime state unavailable") {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("ResolveDoltConnectionTarget() did not reject zombie pid %d", proc.Process.Pid)
}

func TestValidateConnectionConfigStateRejectsWildcardExternalHost(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	if err := ValidateConnectionConfigState(fs, city, city, ConfigState{EndpointOrigin: EndpointOriginCityCanonical, DoltHost: "0.0.0.0", DoltPort: "4406"}); err == nil || !strings.Contains(err.Error(), "bind address") {
		t.Fatalf("ValidateConnectionConfigState() error = %v", err)
	}
	rig := filepath.Join(t.TempDir(), "frontend")
	if err := ValidateConnectionConfigState(fs, city, rig, ConfigState{EndpointOrigin: EndpointOriginExplicit, DoltHost: "::", DoltPort: "4406"}); err == nil || !strings.Contains(err.Error(), "bind address") {
		t.Fatalf("ValidateConnectionConfigState() explicit rig error = %v", err)
	}
}

func TestResolveDoltConnectionTargetRejectsExplicitCityOrigin(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginExplicit,
		EndpointStatus: EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       "4406",
	})
	writeCanonicalMetadata(t, fs, city, "hq")

	if _, err := ResolveDoltConnectionTarget(fs, city, city); err == nil || !strings.Contains(err.Error(), "invalid for city scope") {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v, want city-scope origin rejection", err)
	}
}

func TestResolveDoltConnectionTargetRejectsManagedCityTrackedEndpoint(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
		DoltHost:       "stale-db.example.com",
		DoltPort:       "4406",
		DoltUser:       "stale-user",
	})
	writeCanonicalMetadata(t, fs, city, "hq")
	_ = writeReachableRuntimeState(t, fs, city)

	if _, err := ResolveDoltConnectionTarget(fs, city, city); err == nil || !strings.Contains(err.Error(), "must not track") {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v, want tracked-endpoint rejection", err)
	}
}

func TestResolveDoltConnectionTargetRejectsInheritedRigTrackedEndpointUnderManagedCity(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	})
	writeCanonicalConfig(t, fs, rig, ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: EndpointOriginInheritedCity,
		EndpointStatus: EndpointStatusVerified,
		DoltHost:       "stale-rig-db.example.com",
		DoltPort:       "5507",
		DoltUser:       "stale-user",
	})
	writeCanonicalMetadata(t, fs, rig, "fe")
	writeRuntimeState(t, fs, city, `{"running":true,"port":3311}`)

	if _, err := ResolveDoltConnectionTarget(fs, city, rig); err == nil || !strings.Contains(err.Error(), "must not track") {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v, want inherited managed-city tracked-endpoint rejection", err)
	}
}

func TestResolveDoltConnectionTargetLegacyPortOnlyCityUsesLoopback(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{IssuePrefix: "gc", DoltPort: "4406"})
	writeCanonicalMetadata(t, fs, city, "hq")

	target, err := ResolveDoltConnectionTarget(fs, city, city)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v", err)
	}
	if target.EndpointOrigin != EndpointOriginCityCanonical || target.EndpointStatus != EndpointStatusUnverified {
		t.Fatalf("legacy city target = %+v", target)
	}
	if !target.External || target.Host != "127.0.0.1" || target.Port != "4406" {
		t.Fatalf("target = %+v", target)
	}
}

func TestResolveDoltConnectionTargetRejectsCityCanonicalMissingHost(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginCityCanonical,
		EndpointStatus: EndpointStatusVerified,
		DoltPort:       "4406",
	})
	writeCanonicalMetadata(t, fs, city, "hq")

	if _, err := ResolveDoltConnectionTarget(fs, city, city); err == nil || !strings.Contains(err.Error(), "requires both dolt.host and dolt.port") {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v, want canonical city host+port rejection", err)
	}
}

func TestResolveDoltConnectionTargetRejectsExplicitRigMissingHost(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")
	writeCanonicalConfig(t, fs, rig, ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: EndpointOriginExplicit,
		EndpointStatus: EndpointStatusVerified,
		DoltPort:       "5507",
	})
	writeCanonicalMetadata(t, fs, rig, "fe")

	if _, err := ResolveDoltConnectionTarget(fs, city, rig); err == nil || !strings.Contains(err.Error(), "canonical explicit rig config requires both dolt.host and dolt.port") {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v, want explicit rig host+port rejection", err)
	}
}

func TestResolveDoltConnectionTargetRejectsInheritedExternalRigMissingHost(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginCityCanonical,
		EndpointStatus: EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       "4406",
	})
	writeCanonicalConfig(t, fs, rig, ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: EndpointOriginInheritedCity,
		EndpointStatus: EndpointStatusVerified,
		DoltPort:       "4406",
	})
	writeCanonicalMetadata(t, fs, rig, "fe")

	if _, err := ResolveDoltConnectionTarget(fs, city, rig); err == nil || !strings.Contains(err.Error(), "canonical inherited rig config requires both dolt.host and dolt.port") {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v, want inherited rig host+port rejection", err)
	}
}

func TestValidateCanonicalConfigStateRejectsCityCanonicalWithoutHost(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()

	err := ValidateCanonicalConfigState(fs, city, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginCityCanonical,
		EndpointStatus: EndpointStatusVerified,
		DoltPort:       "3307",
	})
	if err == nil || !strings.Contains(err.Error(), "requires both dolt.host and dolt.port") {
		t.Fatalf("ValidateCanonicalConfigState() error = %v, want missing host rejection", err)
	}
}

func TestValidateCanonicalConfigStateRejectsExplicitRigWithoutHost(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")

	err := ValidateCanonicalConfigState(fs, city, rig, ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: EndpointOriginExplicit,
		EndpointStatus: EndpointStatusVerified,
		DoltPort:       "4406",
	})
	if err == nil || !strings.Contains(err.Error(), "canonical explicit rig config requires both dolt.host and dolt.port") {
		t.Fatalf("ValidateCanonicalConfigState() error = %v, want missing host rejection", err)
	}
}

func TestValidateCanonicalConfigStateAllowsTrackedInheritedRigWithoutCityCanonicalDuringMigration(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")

	err := ValidateCanonicalConfigState(fs, city, rig, ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: EndpointOriginInheritedCity,
		EndpointStatus: EndpointStatusUnverified,
		DoltHost:       "db.example.com",
		DoltPort:       "4406",
	})
	if err != nil {
		t.Fatalf("ValidateCanonicalConfigState() error = %v, want migratable inherited rig", err)
	}
}

func TestValidateCanonicalConfigStateAllowsLegacyPortOnlyRigConfigUnderManagedCity(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	})

	err := ValidateCanonicalConfigState(fs, city, rig, ConfigState{
		IssuePrefix: "fe",
		DoltPort:    "5507",
	})
	if err != nil {
		t.Fatalf("ValidateCanonicalConfigState() error = %v, want legacy managed rig to remain migratable", err)
	}
}

func TestResolveAuthoritativeConfigStateNormalizesLegacyExternalCity(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	if err := fs.MkdirAll(filepath.Join(city, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(filepath.Join(city, ".beads", "config.yaml"), []byte(`issue_prefix: gc
dolt.host: db.example.com
dolt.port: 4406
`), 0o644); err != nil {
		t.Fatal(err)
	}

	state, ok, err := ResolveAuthoritativeConfigState(fs, city, city, "gc")
	if err != nil {
		t.Fatalf("ResolveAuthoritativeConfigState() error = %v", err)
	}
	if !ok {
		t.Fatal("ResolveAuthoritativeConfigState() = not authoritative, want city canonical state")
	}
	if state.EndpointOrigin != EndpointOriginCityCanonical || state.EndpointStatus != EndpointStatusUnverified {
		t.Fatalf("state = %+v", state)
	}
	if state.DoltHost != "db.example.com" || state.DoltPort != "4406" {
		t.Fatalf("state = %+v", state)
	}
}

func TestResolveAuthoritativeConfigStateDerivesInheritedRigFromCityCanonical(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginCityCanonical,
		EndpointStatus: EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       "4406",
		DoltUser:       "city-user",
	})
	writeCanonicalConfig(t, fs, rig, ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: EndpointOriginInheritedCity,
		EndpointStatus: EndpointStatusUnverified,
		DoltHost:       "db.example.com",
		DoltPort:       "4406",
		DoltUser:       "city-user",
	})

	state, ok, err := ResolveAuthoritativeConfigState(fs, city, rig, "fe")
	if err != nil {
		t.Fatalf("ResolveAuthoritativeConfigState() error = %v", err)
	}
	if !ok {
		t.Fatal("ResolveAuthoritativeConfigState() = not authoritative, want inherited state")
	}
	if state.EndpointOrigin != EndpointOriginInheritedCity || state.EndpointStatus != EndpointStatusVerified {
		t.Fatalf("state = %+v", state)
	}
	if state.DoltHost != "db.example.com" || state.DoltPort != "4406" || state.DoltUser != "city-user" {
		t.Fatalf("state = %+v", state)
	}
}

func TestResolveAuthoritativeConfigStateKeepsExplicitRigWithoutCityRuntime(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")
	writeCanonicalConfig(t, fs, rig, ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: EndpointOriginExplicit,
		EndpointStatus: EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       "4406",
	})

	state, ok, err := ResolveAuthoritativeConfigState(fs, city, rig, "fe")
	if err != nil {
		t.Fatalf("ResolveAuthoritativeConfigState() error = %v", err)
	}
	if !ok {
		t.Fatal("ResolveAuthoritativeConfigState() = not authoritative, want explicit rig state")
	}
	if state.EndpointOrigin != EndpointOriginExplicit || state.DoltHost != "db.example.com" || state.DoltPort != "4406" {
		t.Fatalf("state = %+v", state)
	}
}

func TestResolveAuthoritativeConfigStateDerivesLegacyPortOnlyRigUnderManagedCity(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	})
	if err := fs.MkdirAll(filepath.Join(rig, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(filepath.Join(rig, ".beads", "config.yaml"), []byte(`issue_prefix: fe
dolt.port: 5507
`), 0o644); err != nil {
		t.Fatal(err)
	}

	state, ok, err := ResolveAuthoritativeConfigState(fs, city, rig, "fe")
	if err != nil {
		t.Fatalf("ResolveAuthoritativeConfigState() error = %v", err)
	}
	if !ok {
		t.Fatal("ResolveAuthoritativeConfigState() = not authoritative, want inherited managed state")
	}
	if state.EndpointOrigin != EndpointOriginInheritedCity || state.EndpointStatus != EndpointStatusVerified {
		t.Fatalf("state = %+v", state)
	}
}

func TestScopeUsesExplicitEndpointLegacyExplicitRig(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(city, "frontend")
	writeCanonicalConfig(t, fs, city, ConfigState{IssuePrefix: "gc", DoltHost: "db.example.com", DoltPort: "4406"})
	writeCanonicalConfig(t, fs, rig, ConfigState{IssuePrefix: "fe", DoltHost: "other.example.com", DoltPort: "5507"})

	explicit, err := ScopeUsesExplicitEndpoint(fs, city, rig)
	if err != nil {
		t.Fatalf("ScopeUsesExplicitEndpoint() error = %v", err)
	}
	if !explicit {
		t.Fatal("ScopeUsesExplicitEndpoint() = false, want true")
	}
}

func TestAllowsInvalidInheritedCityFallback(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginCityCanonical,
		EndpointStatus: EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       "4406",
	})
	writeCanonicalConfig(t, fs, rig, ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: EndpointOriginInheritedCity,
		EndpointStatus: EndpointStatusVerified,
		DoltHost:       "stale.example.com",
		DoltPort:       "5507",
	})

	fallback, err := AllowsInvalidInheritedCityFallback(fs, city, rig)
	if err != nil {
		t.Fatalf("AllowsInvalidInheritedCityFallback() error = %v", err)
	}
	if !fallback {
		t.Fatal("AllowsInvalidInheritedCityFallback() = false, want true")
	}
}

func TestValidateInheritedCityEndpointMirrorRejectsInvalidInheritedMirror(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginCityCanonical,
		EndpointStatus: EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       "4406",
		DoltUser:       "city-user",
	})
	writeCanonicalConfig(t, fs, rig, ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: EndpointOriginInheritedCity,
		EndpointStatus: EndpointStatusVerified,
		DoltHost:       "stale.example.com",
		DoltPort:       "5507",
		DoltUser:       "city-user",
	})

	err := ValidateInheritedCityEndpointMirror(fs, city, rig)
	if err == nil || !strings.Contains(err.Error(), "must mirror the city endpoint") {
		t.Fatalf("ValidateInheritedCityEndpointMirror() error = %v, want inherited mirror rejection", err)
	}
}

func TestResolveScopeConfigStateMissing(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()

	resolved, err := ResolveScopeConfigState(fs, city, city, "gc")
	if err != nil {
		t.Fatalf("ResolveScopeConfigState() error = %v", err)
	}
	if resolved.Kind != ScopeConfigMissing {
		t.Fatalf("ResolveScopeConfigState().Kind = %q, want %q", resolved.Kind, ScopeConfigMissing)
	}
}

func TestResolveScopeConfigStateLegacyMinimal(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	if err := fs.MkdirAll(filepath.Join(city, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(filepath.Join(city, ".beads", "config.yaml"), []byte(`issue_prefix: gc
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	resolved, err := ResolveScopeConfigState(fs, city, city, "gc")
	if err != nil {
		t.Fatalf("ResolveScopeConfigState() error = %v", err)
	}
	if resolved.Kind != ScopeConfigLegacyMinimal {
		t.Fatalf("ResolveScopeConfigState().Kind = %q, want %q", resolved.Kind, ScopeConfigLegacyMinimal)
	}
}

func TestResolveScopeConfigStateNormalizesLegacyExternalCity(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	if err := fs.MkdirAll(filepath.Join(city, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(filepath.Join(city, ".beads", "config.yaml"), []byte(`issue_prefix: gc
dolt.host: db.example.com
dolt.port: 4406
`), 0o644); err != nil {
		t.Fatal(err)
	}

	resolved, err := ResolveScopeConfigState(fs, city, city, "gc")
	if err != nil {
		t.Fatalf("ResolveScopeConfigState() error = %v", err)
	}
	if resolved.Kind != ScopeConfigAuthoritative {
		t.Fatalf("ResolveScopeConfigState().Kind = %q, want %q", resolved.Kind, ScopeConfigAuthoritative)
	}
	if resolved.State.EndpointOrigin != EndpointOriginCityCanonical || resolved.State.EndpointStatus != EndpointStatusUnverified {
		t.Fatalf("ResolveScopeConfigState().State = %+v", resolved.State)
	}
}

func TestResolveScopeConfigStateNormalizesLegacyManagedRigPortResidue(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	})
	if err := fs.MkdirAll(filepath.Join(rig, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(filepath.Join(rig, ".beads", "config.yaml"), []byte(`issue_prefix: fe
dolt.port: 5507
`), 0o644); err != nil {
		t.Fatal(err)
	}

	resolved, err := ResolveScopeConfigState(fs, city, rig, "fe")
	if err != nil {
		t.Fatalf("ResolveScopeConfigState() error = %v", err)
	}
	if resolved.Kind != ScopeConfigAuthoritative {
		t.Fatalf("ResolveScopeConfigState().Kind = %q, want %q", resolved.Kind, ScopeConfigAuthoritative)
	}
	if resolved.State.EndpointOrigin != EndpointOriginInheritedCity || resolved.State.EndpointStatus != EndpointStatusVerified {
		t.Fatalf("ResolveScopeConfigState().State = %+v", resolved.State)
	}
}

//nolint:unparam // helper keeps FS explicit in tests
func writeCanonicalConfig(t *testing.T, fs fsys.FS, dir string, state ConfigState) {
	t.Helper()
	if err := fs.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureCanonicalConfig(fs, filepath.Join(dir, ".beads", "config.yaml"), state); err != nil {
		t.Fatal(err)
	}
}

//nolint:unparam // helper keeps FS explicit in tests
func writeCanonicalMetadata(t *testing.T, fs fsys.FS, dir, db string) {
	t.Helper()
	if _, err := EnsureCanonicalMetadata(fs, filepath.Join(dir, ".beads", "metadata.json"), MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: db,
	}); err != nil {
		t.Fatal(err)
	}
}

//nolint:unparam // helper keeps FS explicit for symmetry with related helpers
func writeReachableRuntimeState(t *testing.T, fs fsys.FS, city string) string {
	t.Helper()
	return writeReachableRuntimeStateWithDataDir(t, fs, city, filepath.Join(city, ".beads", "dolt"))
}

func writeReachableRuntimeStateWithDataDir(t *testing.T, fs fsys.FS, city, dataDir string) string {
	t.Helper()
	return writeReachableRuntimeStateOnHostWithPIDAndDataDir(t, fs, city, "127.0.0.1", os.Getpid(), dataDir)
}

func writeReachableRuntimeStateOnHost(t *testing.T, fs fsys.FS, city, host string) string {
	t.Helper()
	return writeReachableRuntimeStateOnHostWithPID(t, fs, city, host, os.Getpid())
}

func writeReachableRuntimeStateOnHostWithPID(t *testing.T, fs fsys.FS, city, host string, pid int) string {
	t.Helper()
	return writeReachableRuntimeStateOnHostWithPIDAndDataDir(t, fs, city, host, pid, filepath.Join(city, ".beads", "dolt"))
}

func writeReachableRuntimeStateOnHostWithPIDAndDataDir(t *testing.T, fs fsys.FS, city, host string, pid int, dataDir string) string {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		// Some hosts aren't bindable on every OS — notably, darwin doesn't
		// auto-alias 127.0.0.0/8 secondary loopbacks (127.0.0.2+) to lo0 the
		// way linux does, so `net.Listen("tcp", "127.0.0.2:0")` fails with
		// "can't assign requested address". The test's premise needs a real
		// listener on this host; without one, the reachability probe in
		// validManagedRuntimeState would (correctly) reject the state. Skip
		// rather than fail so the negative coverage on linux is preserved
		// without forcing a macOS-only flake. Sibling tests that exercise
		// the same env-override path against a routable host
		// (reachableNonLoopbackHost) and a non-routable host (TEST-NET-1)
		// still run on every OS.
		t.Skipf("cannot bind %s: %v (typical on darwin where 127.0.0.0/8 secondary loopback aliases aren't installed by default)", net.JoinHostPort(host, "0"), err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port
	writeRuntimeState(t, fs, city, fmt.Sprintf(`{"running":true,"pid":%d,"port":%d,"data_dir":%q}`, pid, port, dataDir))
	return fmt.Sprintf("%d", port)
}

func reachableNonLoopbackHost(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range addrs {
		var ip net.IP
		switch typed := addr.(type) {
		case *net.IPNet:
			ip = typed.IP
		case *net.IPAddr:
			ip = typed.IP
		default:
			continue
		}
		ip = ip.To4()
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
			continue
		}
		listener, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
		if err != nil {
			continue
		}
		_ = listener.Close()
		return ip.String()
	}
	t.Skip("no bindable non-loopback IPv4 address")
	return ""
}

//nolint:unparam // helper keeps FS explicit in tests
func writeRuntimeState(t *testing.T, fs fsys.FS, city, raw string) {
	t.Helper()
	path := filepath.Join(city, ".gc", "runtime", "packs", "dolt")
	if err := fs.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(raw)) {
		t.Fatalf("writeRuntimeState raw JSON invalid: %s", raw)
	}
	if err := fs.WriteFile(filepath.Join(path, "dolt-state.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestManagedCityHost_Default asserts the default remains 127.0.0.1 when the
// env override is unset or empty.
func TestManagedCityHost_Default(t *testing.T) {
	t.Setenv(ManagedCityHostEnv, "")
	if got := managedCityHost(); got != "127.0.0.1" {
		t.Fatalf("managedCityHost() = %q, want 127.0.0.1", got)
	}
}

// TestManagedCityHost_EnvOverride asserts GC_DOLT_HOST overrides the
// default loopback — this is how containerised callers (MCP servers, proxies
// on Docker Desktop) redirect away from the container's own 127.0.0.1.
func TestManagedCityHost_EnvOverride(t *testing.T) {
	t.Setenv(ManagedCityHostEnv, "host.docker.internal")
	if got := managedCityHost(); got != "host.docker.internal" {
		t.Fatalf("managedCityHost() = %q, want host.docker.internal", got)
	}
}

// TestManagedCityHost_EnvTrimmed asserts surrounding whitespace is trimmed so
// "  host  " → "host". Mirrors how other config values are normalised in this
// package.
func TestManagedCityHost_EnvTrimmed(t *testing.T) {
	t.Setenv(ManagedCityHostEnv, "  example.internal  ")
	if got := managedCityHost(); got != "example.internal" {
		t.Fatalf("managedCityHost() = %q, want example.internal", got)
	}
}

// TestResolveDoltConnectionTargetManagedCity_EnvOverride asserts that a
// managed-city resolve honors GC_DOLT_HOST with a value distinguishable from
// the unset default.
func TestResolveDoltConnectionTargetManagedCity_EnvOverride(t *testing.T) {
	t.Setenv(ManagedCityHostEnv, "127.0.0.2")
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	})
	writeCanonicalMetadata(t, fs, city, "hq")
	port := writeReachableRuntimeStateOnHost(t, fs, city, "127.0.0.2")

	target, err := ResolveDoltConnectionTarget(fs, city, city)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v", err)
	}
	if target.Host != "127.0.0.2" || target.Port != port {
		t.Fatalf("target = %+v, want host 127.0.0.2 port %q", target, port)
	}
}

// TestResolveDoltConnectionTargetManagedCity_EnvOverrideAppliesToTarget sets
// the env to an invalid host and asserts the liveness check fails — proving
// the override reaches the reachability probe, not just the returned target.
// If the probe were still hardcoded to 127.0.0.1, it would succeed (the
// listener is on loopback) and this test would fail.
func TestResolveDoltConnectionTargetManagedCity_EnvOverrideAppliesToReachability(t *testing.T) {
	// Use a non-routable TEST-NET-1 address so DialTimeout fails fast.
	t.Setenv(ManagedCityHostEnv, "192.0.2.1")
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	})
	writeCanonicalMetadata(t, fs, city, "hq")
	writeReachableRuntimeState(t, fs, city)

	_, err := ResolveDoltConnectionTarget(fs, city, city)
	if err == nil || !strings.Contains(err.Error(), "dolt runtime state unavailable") {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v, want unavailable (override routed liveness probe elsewhere)", err)
	}
}
