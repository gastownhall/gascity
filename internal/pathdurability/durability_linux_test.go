//go:build linux

package pathdurability

import (
	"os"
	"path/filepath"
	"testing"
)

// TestClassifyOnRealFilesystem exercises the real syscall probes rather than the
// fake mount table, so a bug in the platform file cannot hide behind the double.
// Linux-only: probe_other.go is a deliberate Unknown stub for every other
// platform (see its doc comment), so an assertion against the real probe's
// result is only ever true here.
func TestClassifyOnRealFilesystem(t *testing.T) {
	cityRoot := t.TempDir()

	t.Run("same device as the city root", func(t *testing.T) {
		rigPath := filepath.Join(cityRoot, "rigs", "project")
		if got := Classify(cityRoot, rigPath); got.Class != CityDevice {
			t.Fatalf("Classify(%q, %q).Class = %q, want %q", cityRoot, rigPath, got.Class, CityDevice)
		}
	})

	t.Run("tmpfs elsewhere", func(t *testing.T) {
		// /dev/shm is tmpfs on every Linux host and needs no privilege to read.
		if _, err := os.Stat("/dev/shm"); err != nil {
			t.Skip("no /dev/shm on this host")
		}
		if sameDevice(t, cityRoot, "/dev/shm") {
			t.Skip("city temp dir is itself on /dev/shm's device; same-device rule applies")
		}
		got := Classify(cityRoot, "/dev/shm/gc-durability-probe")
		if got.Class != Ephemeral {
			t.Fatalf("Classify(%q, /dev/shm/...).Class = %q, want %q", cityRoot, got.Class, Ephemeral)
		}
		if got.Filesystem != "tmpfs" {
			t.Fatalf("Filesystem = %q, want tmpfs", got.Filesystem)
		}
	})
}

func sameDevice(t *testing.T, a, b string) bool {
	t.Helper()
	devA, err := deviceID(a)
	if err != nil {
		t.Skipf("deviceID(%q): %v", a, err)
	}
	devB, err := deviceID(b)
	if err != nil {
		t.Skipf("deviceID(%q): %v", b, err)
	}
	return devA == devB
}
