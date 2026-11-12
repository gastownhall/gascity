//go:build linux

package proctable

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

const testUserManager = "/user.slice/user-1000.slice/user@1000.service"

// writeTestCgroup writes pid's cgroup file under a procfs-shaped root.
func writeTestCgroup(t *testing.T, root string, pid int, content string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDetachedUserServiceUnitReadsInjectedCgroup(t *testing.T) {
	root := t.TempDir()
	writeTestCgroup(t, root, 100, "0::"+testUserManager+"/app.slice/devserver.service\n")
	writeTestCgroup(t, root, 101, "0::"+testUserManager+"/app.slice/tmux-spawn-1.scope\n")
	writeTestCgroup(t, root, 102, "garbage\n")
	writeTestCgroup(t, root, 103, "1:name=systemd:"+testUserManager+"/app.slice/podman.service/payload\n")
	if err := os.MkdirAll(filepath.Join(root, "104"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Without an injected root the probe never reads the host's live /proc.
	if got := DetachedUserServiceUnit(os.Getpid()); got != "" {
		t.Fatalf("DetachedUserServiceUnit read the live /proc under go test: %q", got)
	}
	SetScanRootForTesting(t, root)
	for pid, want := range map[int]string{
		100: "devserver.service",
		101: "",
		102: "",
		103: "podman.service",
		104: "", // no cgroup file
		105: "", // no such process
		0:   "",
		-1:  "",
	} {
		if got := DetachedUserServiceUnit(pid); got != want {
			t.Errorf("DetachedUserServiceUnit(%d) = %q, want %q", pid, got, want)
		}
	}
}

// TestDetachedUserServiceUnitSparesOwnAndSupervisorUnits: the probe fences
// only a foreign user service. A root in the reaping process's own unit, or in
// Gas City's default or suffixed supervisor unit, is a stale process of Gas
// City's own and stays reapable; a lookalike name or the same unit name under
// another user manager is foreign. Without a readable self cgroup no root
// matches the own unit, but the supervisor exclusions still apply.
func TestDetachedUserServiceUnitSparesOwnAndSupervisorUnits(t *testing.T) {
	const otherManager = "/user.slice/user-1001.slice/user@1001.service"
	cases := map[int]struct {
		cgroup   string
		withSelf string
		noSelf   string
	}{
		200: {cgroup: testUserManager + "/app.slice/reaper.service", withSelf: "", noSelf: "reaper.service"},
		201: {cgroup: testUserManager + "/app.slice/reaper.service/payload", withSelf: "", noSelf: "reaper.service"},
		202: {cgroup: otherManager + "/app.slice/reaper.service", withSelf: "reaper.service", noSelf: "reaper.service"},
		203: {cgroup: testUserManager + "/app.slice/devserver.service", withSelf: "devserver.service", noSelf: "devserver.service"},
		204: {cgroup: testUserManager + "/app.slice/" + SupervisorSystemdUnitStem + ".service", withSelf: "", noSelf: ""},
		205: {cgroup: testUserManager + "/" + SupervisorSystemdUnitStem + "-3f9a2c.service", withSelf: "", noSelf: ""},
		206: {cgroup: testUserManager + "/app.slice/" + SupervisorSystemdUnitStem + "x.service", withSelf: SupervisorSystemdUnitStem + "x.service", noSelf: SupervisorSystemdUnitStem + "x.service"},
		207: {cgroup: testUserManager + "/app.slice/my-" + SupervisorSystemdUnitStem + ".service", withSelf: "my-" + SupervisorSystemdUnitStem + ".service", noSelf: "my-" + SupervisorSystemdUnitStem + ".service"},
		208: {cgroup: testUserManager + "/app.slice/" + SupervisorSystemdUnitStem + "-.service", withSelf: SupervisorSystemdUnitStem + "-.service", noSelf: SupervisorSystemdUnitStem + "-.service"},
	}
	for _, withSelf := range []bool{true, false} {
		root := t.TempDir()
		for pid, tc := range cases {
			writeTestCgroup(t, root, pid, "0::"+tc.cgroup+"\n")
		}
		if withSelf {
			writeTestCgroup(t, root, os.Getpid(), "0::"+testUserManager+"/app.slice/reaper.service\n")
		}
		SetScanRootForTesting(t, root)
		for pid, tc := range cases {
			want := tc.noSelf
			if withSelf {
				want = tc.withSelf
			}
			if got := DetachedUserServiceUnit(pid); got != want {
				t.Errorf("withSelf=%v: DetachedUserServiceUnit(%d) [%s] = %q, want %q", withSelf, pid, tc.cgroup, got, want)
			}
		}
	}
}
