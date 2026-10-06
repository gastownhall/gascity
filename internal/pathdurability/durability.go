// Package pathdurability classifies whether a filesystem path is likely to
// survive the process that created it.
//
// A city's rig directory must outlive the container or host it was registered
// from. When it does not — a rig registered under /tmp inside a Kubernetes pod,
// say — the registration looks healthy right up until the pod is replaced, at
// which point the rig content is gone with no remote and no way back.
//
// The classification is deliberately positive-first: a path on the same device
// as the city root is durable by construction, because if that device were not
// durable the city itself would not survive either. Only paths on a *different*
// device are interrogated further, and only filesystems that are ephemeral by
// construction (tmpfs, ramfs, an overlay container rootfs) are called out.
// Anything else — a second PVC, an NFS mount, a second disk — is reported as
// unclassifiable rather than condemned.
package pathdurability

import (
	"path/filepath"
)

// Class is the durability verdict for a path.
type Class string

const (
	// Unknown means the probe could not reach a conclusion: the platform has no
	// probe, or the path (and every ancestor) could not be stat'd. Callers must
	// treat Unknown as "no opinion" and never refuse on it.
	Unknown Class = "unknown"
	// CityDevice means the path lives on the same filesystem device as the city
	// root, so it is exactly as durable as the city itself.
	CityDevice Class = "city-device"
	// Ephemeral means the path lives on a filesystem that cannot outlive the
	// current container or boot.
	Ephemeral Class = "ephemeral"
	// OtherDevice means the path lives on some other device that is not
	// ephemeral by construction. It may well be durable; the probe cannot tell.
	OtherDevice Class = "other-device"
)

// Result is the outcome of a Classify call.
type Result struct {
	// Class is the verdict.
	Class Class
	// Filesystem names the ephemeral filesystem when Class is Ephemeral.
	Filesystem string
	// Probed is the path the verdict was actually drawn from. When the target
	// does not exist yet, this is its nearest existing ancestor — the device the
	// directory would be created on.
	Probed string
}

// Linux superblock magic numbers for the filesystems that cannot outlive their
// container or boot. These are stable kernel ABI values, so they are declared
// here rather than in the platform file: it keeps the classification table and
// its tests portable, while the raw statfs call stays behind a build tag.
const (
	magicTmpfs     = 0x01021994
	magicRamfs     = 0x858458f6
	magicOverlayfs = 0x794c7630
)

// ephemeralFilesystems maps a superblock magic to the name shown to operators.
//
// tmpfs covers /tmp on a systemd host, /dev/shm, and a Kubernetes emptyDir
// declared with medium: Memory. A default emptyDir is a kubelet-host directory
// bind-mounted into the pod, so statfs reports the host filesystem instead and
// it lands in OtherDevice — warned about, not refused. overlayfs covers the
// container rootfs, which is what makes an in-pod path like /var/tmp ephemeral
// even though it is not /tmp — the reason a prefix denylist cannot do this job.
var ephemeralFilesystems = map[uint32]string{
	magicTmpfs:     "tmpfs",
	magicRamfs:     "ramfs",
	magicOverlayfs: "overlayfs",
}

// Injectable probes. Production values come from the platform build-tagged
// files; tests replace them to drive each rule independently.
var (
	deviceIDFunc       = deviceID
	filesystemTypeFunc = filesystemType
	evalSymlinksFunc   = filepath.EvalSymlinks
)

// Classify reports whether path is likely to survive replacement of the process
// that registered it, judged relative to cityRoot.
//
// A relative path is resolved against cityRoot, matching how the rest of the
// codebase reads a rig path out of city.toml. Resolving it against the process
// working directory instead would produce a confident verdict about an unrelated
// directory: probeDevice's ancestor walk terminates at "." rather than running
// out of ancestors, so such a path reaches Unknown only if stat(".") itself
// fails, and the fail-open rule below would not catch the mistake.
//
// It never returns an error: a probe that cannot reach a conclusion yields
// Unknown, so a caller on an unsupported platform or an unreadable mount is
// never blocked by a check that could not run.
func Classify(cityRoot, path string) Result {
	if !filepath.IsAbs(path) {
		path = filepath.Join(cityRoot, path)
	}

	cityDev, _, cityErr := probeDevice(resolveSymlinks(cityRoot))

	dev, probed, err := probeDevice(resolveSymlinks(path))
	if err != nil {
		return Result{Class: Unknown, Probed: probed}
	}

	// Positive rule first: same device as the city root is durable by
	// construction. This is what keeps the check silent for a whole city that
	// deliberately lives on tmpfs (a test fixture, a disposable dev city)
	// instead of condemning every path on a filesystem it dislikes.
	if cityErr == nil && dev == cityDev {
		return Result{Class: CityDevice, Probed: probed}
	}

	fsType, fsErr := filesystemTypeFunc(probed)
	if fsErr != nil {
		return Result{Class: Unknown, Probed: probed}
	}
	if name, ok := ephemeralFilesystems[fsType]; ok {
		return Result{Class: Ephemeral, Filesystem: name, Probed: probed}
	}
	return Result{Class: OtherDevice, Probed: probed}
}

// probeDevice returns the device ID of path, walking up to the nearest existing
// ancestor when path itself does not exist yet. `gc rig add` accepts a rig
// directory that has not been created, and the device it would be created on is
// the device of its parent.
func probeDevice(path string) (dev uint64, probed string, err error) {
	probed = path
	for {
		dev, err = deviceIDFunc(probed)
		if err == nil {
			return dev, probed, nil
		}
		parent := filepath.Dir(probed)
		if parent == probed {
			return 0, probed, err
		}
		probed = parent
	}
}

// resolveSymlinks returns the fully resolved path, falling back to the input
// when resolution fails. A path that cannot be resolved is still worth probing:
// probeDevice will walk up to an ancestor that exists.
func resolveSymlinks(path string) string {
	resolved, err := evalSymlinksFunc(path)
	if err != nil {
		return path
	}
	return resolved
}
