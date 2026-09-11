package pathdurability

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeMounts is a test double for the two syscall probes: it maps a path prefix
// to the device and filesystem magic that path reports.
type fakeMounts struct {
	// mounts maps a mount point to (device, magic). The longest matching prefix
	// wins, mirroring how the kernel resolves a path to a superblock.
	mounts map[string]struct {
		dev   uint64
		magic uint32
	}
	// missing lists paths that do not exist, so the ancestor walk is exercised.
	missing map[string]bool
}

func (f *fakeMounts) lookup(path string) (dev uint64, magic uint32, err error) {
	if f.missing[path] {
		return 0, 0, os.ErrNotExist
	}
	best := ""
	for mount := range f.mounts {
		if mount != "/" && path != mount && !hasPathPrefix(path, mount) {
			continue
		}
		if len(mount) > len(best) {
			best = mount
		}
	}
	if best == "" {
		return 0, 0, os.ErrNotExist
	}
	m := f.mounts[best]
	return m.dev, m.magic, nil
}

func hasPathPrefix(path, prefix string) bool {
	if prefix == "/" {
		return true
	}
	return len(path) > len(prefix) && path[:len(prefix)] == prefix && path[len(prefix)] == '/'
}

// install wires the fake into the package probes for the duration of the test.
func (f *fakeMounts) install(t *testing.T) {
	t.Helper()
	origDev, origFS, origEval := deviceIDFunc, filesystemTypeFunc, evalSymlinksFunc
	t.Cleanup(func() {
		deviceIDFunc, filesystemTypeFunc, evalSymlinksFunc = origDev, origFS, origEval
	})
	deviceIDFunc = func(p string) (uint64, error) {
		dev, _, err := f.lookup(p)
		return dev, err
	}
	filesystemTypeFunc = func(p string) (uint32, error) {
		_, magic, err := f.lookup(p)
		return magic, err
	}
	// Identity resolution: the fake mount table already speaks in resolved paths.
	evalSymlinksFunc = func(p string) (string, error) { return p, nil }
}

const (
	devPVC  = 100
	devRoot = 200
	devTmp  = 300
	devData = 400

	magicExt4 = 0xEF53
)

// hostedPodMounts models the filesystem an in-pod controller actually sees: a
// durable city PVC at /city, an overlay container rootfs at /, and a tmpfs /tmp.
func hostedPodMounts() *fakeMounts {
	return &fakeMounts{
		mounts: map[string]struct {
			dev   uint64
			magic uint32
		}{
			"/":        {devRoot, magicOverlayfs},
			"/city":    {devPVC, magicExt4},
			"/tmp":     {devTmp, magicTmpfs},
			"/dev/shm": {devTmp + 1, magicTmpfs},
		},
	}
}

// TestClassifyAcceptsCityRootedBindings is the regression direction: every rig
// path shape a healthy hosted city uses must stay classified as durable. A guard
// that refuses any of these is worse than no guard at all.
func TestClassifyAcceptsCityRootedBindings(t *testing.T) {
	mounts := hostedPodMounts()
	mounts.install(t)

	const cityRoot = "/city"
	// Both layouts a hosted controller produces: nested under rigs/, and top-level.
	for _, rigPath := range []string{
		"/city/rigs/backend",
		"/city/rigs/frontend",
		"/city/project",
		"/city/project2",
	} {
		t.Run(rigPath, func(t *testing.T) {
			got := Classify(cityRoot, rigPath)
			if got.Class != CityDevice {
				t.Fatalf("Classify(%q, %q).Class = %q, want %q", cityRoot, rigPath, got.Class, CityDevice)
			}
		})
	}
}

// TestClassifyResolvesRelativePathsAgainstTheCityRoot covers the shape city.toml
// actually allows: a rig path written relative to the city, which the rest of the
// codebase joins onto the city root (see internal/rig/topology.go). Judged against
// the process working directory instead, such a path yields a confident verdict
// about an unrelated directory — and because probeDevice's ancestor walk
// terminates at "." rather than running out of ancestors, it falls back to
// Unknown only if stat(".") itself fails, so in practice the fail-open escape
// hatch does not save it.
func TestClassifyResolvesRelativePathsAgainstTheCityRoot(t *testing.T) {
	mounts := hostedPodMounts()
	mounts.install(t)

	t.Run("relative path under the city is durable", func(t *testing.T) {
		got := Classify("/city", "rigs/backend")
		if got.Class != CityDevice {
			t.Fatalf("Classify(/city, rigs/backend).Class = %q, want %q", got.Class, CityDevice)
		}
		if got.Probed != "/city/rigs/backend" {
			t.Fatalf("Probed = %q, want /city/rigs/backend", got.Probed)
		}
	})

	// Control: relative paths must not be blanket-accepted. One that escapes the
	// city onto tmpfs still has to be caught, which a fix that simply trusted any
	// non-absolute path would miss.
	t.Run("relative path escaping onto tmpfs is still caught", func(t *testing.T) {
		got := Classify("/city", "../tmp/adopt")
		if got.Class != Ephemeral {
			t.Fatalf("Classify(/city, ../tmp/adopt).Class = %q, want %q", got.Class, Ephemeral)
		}
		if got.Filesystem != "tmpfs" {
			t.Fatalf("Filesystem = %q, want tmpfs", got.Filesystem)
		}
	})
}

// TestClassifyRejectsNonPersistentPaths is the guard direction. /var/tmp is the
// case a "/tmp" prefix denylist misses: inside a pod it is on the overlay
// rootfs and dies with the container just the same.
func TestClassifyRejectsNonPersistentPaths(t *testing.T) {
	mounts := hostedPodMounts()
	mounts.install(t)

	tests := []struct {
		name       string
		rigPath    string
		filesystem string
	}{
		{"tmp is tmpfs", "/tmp/adopt", "tmpfs"},
		{"dev shm is tmpfs", "/dev/shm/rig", "tmpfs"},
		{"var tmp is the container rootfs", "/var/tmp/rig", "overlayfs"},
		{"home is the container rootfs", "/home/agent/rig", "overlayfs"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify("/city", tc.rigPath)
			if got.Class != Ephemeral {
				t.Fatalf("Classify(/city, %q).Class = %q, want %q", tc.rigPath, got.Class, Ephemeral)
			}
			if got.Filesystem != tc.filesystem {
				t.Fatalf("Classify(/city, %q).Filesystem = %q, want %q", tc.rigPath, got.Filesystem, tc.filesystem)
			}
		})
	}
}

// TestClassifyAllowsCityOnTheSameEphemeralDevice is the control that proves the
// guard is not simply "reject all tmpfs". A city that itself lives on tmpfs (a
// test fixture, a disposable dev city) is no less durable than its own rigs, so
// the positive same-device rule must win over the ephemeral-filesystem rule.
func TestClassifyAllowsCityOnTheSameEphemeralDevice(t *testing.T) {
	mounts := hostedPodMounts()
	mounts.install(t)

	got := Classify("/tmp/city", "/tmp/city/rigs/project")
	if got.Class != CityDevice {
		t.Fatalf("Classify(/tmp/city, /tmp/city/rigs/project).Class = %q, want %q", got.Class, CityDevice)
	}
}

// TestClassifyReportsOtherDeviceWithoutCondemningIt covers a second durable
// mount — another PVC, an NFS share, a second disk. The probe cannot prove it
// survives, and must not pretend otherwise in either direction.
func TestClassifyReportsOtherDeviceWithoutCondemningIt(t *testing.T) {
	mounts := hostedPodMounts()
	mounts.mounts["/data"] = struct {
		dev   uint64
		magic uint32
	}{devData, magicExt4}
	mounts.install(t)

	got := Classify("/city", "/data/projects/rig")
	if got.Class != OtherDevice {
		t.Fatalf("Classify(/city, /data/projects/rig).Class = %q, want %q", got.Class, OtherDevice)
	}
}

// TestClassifyProbesNearestExistingAncestor covers `gc rig add <new-dir>`: the
// directory does not exist yet, so the verdict must come from the device it
// would be created on.
func TestClassifyProbesNearestExistingAncestor(t *testing.T) {
	mounts := hostedPodMounts()
	mounts.missing = map[string]bool{
		"/tmp/adopt":         true,
		"/tmp/adopt/nested":  true,
		"/city/rigs/brand":   true,
		"/city/rigs/brand/x": true,
	}
	mounts.install(t)

	ephemeral := Classify("/city", "/tmp/adopt/nested")
	if ephemeral.Class != Ephemeral {
		t.Fatalf("missing path under tmpfs: Class = %q, want %q", ephemeral.Class, Ephemeral)
	}
	if ephemeral.Probed != "/tmp" {
		t.Fatalf("missing path under tmpfs: Probed = %q, want /tmp", ephemeral.Probed)
	}

	durable := Classify("/city", "/city/rigs/brand/x")
	if durable.Class != CityDevice {
		t.Fatalf("missing path under the city PVC: Class = %q, want %q", durable.Class, CityDevice)
	}
}

// TestClassifyFailsOpen proves the check never blocks when it cannot run: an
// unprobeable path, and a platform with no probe at all, both yield Unknown.
func TestClassifyFailsOpen(t *testing.T) {
	origDev, origFS, origEval := deviceIDFunc, filesystemTypeFunc, evalSymlinksFunc
	t.Cleanup(func() {
		deviceIDFunc, filesystemTypeFunc, evalSymlinksFunc = origDev, origFS, origEval
	})
	evalSymlinksFunc = func(p string) (string, error) { return p, nil }

	t.Run("no probe on this platform", func(t *testing.T) {
		deviceIDFunc = func(string) (uint64, error) { return 0, errors.New("unsupported platform") }
		filesystemTypeFunc = func(string) (uint32, error) { return 0, errors.New("unsupported platform") }
		if got := Classify("/city", "/tmp/adopt"); got.Class != Unknown {
			t.Fatalf("Class = %q, want %q", got.Class, Unknown)
		}
	})

	t.Run("device readable but filesystem type is not", func(t *testing.T) {
		deviceIDFunc = func(p string) (uint64, error) {
			if p == "/city" {
				return devPVC, nil
			}
			return devTmp, nil
		}
		filesystemTypeFunc = func(string) (uint32, error) { return 0, errors.New("statfs refused") }
		if got := Classify("/city", "/tmp/adopt"); got.Class != Unknown {
			t.Fatalf("Class = %q, want %q", got.Class, Unknown)
		}
	})
}

// TestClassifyOnRealFilesystem exercises the real syscall probes rather than the
// fake mount table, so a bug in the platform file cannot hide behind the double.
//
// Only Linux has those probes: probe_linux.go is `//go:build linux` and
// probe_other.go covers `!linux` by answering errUnsupported, which is what
// makes Classify fail open to Unknown on every other platform. The GOOS check
// below mirrors exactly that build constraint. It deliberately does not skip on
// a probe *error*, which on Linux is a real regression this test must still
// catch — the platform coverage is the only thing being waived here.
func TestClassifyOnRealFilesystem(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("no real durability probe on %s; Classify is contractually Unknown there", runtime.GOOS)
	}
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

// TestEphemeralMagicsSurviveSignedStatfsType pins the conversion filesystemType
// performs. Statfs_t.Type is signed and its width is arch-dependent (int64 on
// amd64/arm64, int32 on 386/arm), while superblock magics are 32-bit unsigned.
// Converting straight to a signed type sign-extends every magic with the high
// bit set, so ramfs (0x858458f6) arrives as a negative number on 386/arm and
// matches nothing in ephemeralFilesystems — the guard silently stops
// recognizing ramfs on exactly those arches. Routing through uint32 truncates
// to the low 32 bits on every arch instead.
func TestEphemeralMagicsSurviveSignedStatfsType(t *testing.T) {
	for magic, name := range ephemeralFilesystems {
		// The two shapes the kernel hands back, per arch.
		wide := int64(magic)
		narrow := int32(magic)

		if got := uint32(wide); got != magic {
			t.Errorf("%s: uint32(int64(%#x)) = %#x, want %#x", name, magic, got, magic)
		}
		if got := uint32(narrow); got != magic {
			t.Errorf("%s: uint32(int32(%#x)) = %#x, want %#x", name, magic, got, magic)
		}
		if _, ok := ephemeralFilesystems[uint32(narrow)]; !ok {
			t.Errorf("%s (%#x) is unreachable from a 32-bit Statfs_t.Type", name, magic)
		}
	}

	// Control: the bug this pins is not hypothetical for every magic — it only
	// bites the ones with the high bit set, and ramfs is one of them. If this
	// stops being true the loop above has lost its teeth.
	var ramfs uint32 = magicRamfs
	if int32(ramfs) >= 0 {
		t.Fatalf("magicRamfs %#x no longer has the high bit set, so the sign-extension control proves nothing", ramfs)
	}
}
