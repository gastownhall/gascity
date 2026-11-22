package contract

import (
	"runtime/debug"
	"strings"

	"golang.org/x/mod/semver"
)

// BeadsBuild is the beads module a Go binary was built from, as its build
// info records it: a bd carries beads as its main module, gc as a
// dependency.
type BeadsBuild struct {
	// Version is the module version build info records (a replacement's
	// own version for a replaced module), empty when it records none.
	Version string
	// Unconfirmed names why Version cannot be compared with a beads
	// release, and is empty when it can.
	Unconfirmed string
}

// ReadBeadsBuild reads which beads module the binary with this build info
// was built from, by the rules the native-store preflight applies to the
// beads library gc links (linkedBeadsLibraryFrom, unconfirmableReason).
func ReadBeadsBuild(info *debug.BuildInfo) BeadsBuild {
	if beadsModuleIn(info) == nil {
		return BeadsBuild{Unconfirmed: "no beads module"}
	}
	library := linkedBeadsLibraryFrom(info)
	return BeadsBuild{Version: library.Version, Unconfirmed: library.unconfirmableReason()}
}

// BDVersionRelation is how the beads version of a bd relates to the version
// of the beads library gc links.
type BDVersionRelation int

const (
	// BDVersionMatches is the linked version itself.
	BDVersionMatches BDVersionRelation = iota
	// BDVersionNewer is a newer release of the linked major version, which
	// is compatible (newerSemverCompatibleBD, gastownhall/gascity#5164).
	BDVersionNewer
	// BDVersionSamePrerelease is another prerelease of the release the
	// linked version is a prerelease of (samePrereleaseSeries).
	BDVersionSamePrerelease
	// BDVersionOlder is older than the linked version. Such a bd refuses to
	// read a store once the store is migrated past the schema it knows.
	BDVersionOlder
	// BDVersionNewerMajor is a newer major version than the linked one,
	// which semver does not promise is compatible.
	BDVersionNewerMajor
	// BDVersionUnparsed is a version that differs from the linked one where
	// either is not semver, so nothing but the difference is known.
	BDVersionUnparsed
)

// Compatible reports whether the version policy accepts a bd in relation r.
func (r BDVersionRelation) Compatible() bool {
	switch r {
	case BDVersionMatches, BDVersionNewer, BDVersionSamePrerelease:
		return true
	default:
		return false
	}
}

// RelateBDVersion relates the beads version of a bd to the version of the
// beads library gc links, ignoring a leading "v" on either. It is the policy
// checkBDVersionHint applies to the version bd reports for itself.
func RelateBDVersion(bdVersion, libraryVersion string) BDVersionRelation {
	if strings.TrimPrefix(bdVersion, "v") == strings.TrimPrefix(libraryVersion, "v") {
		return BDVersionMatches
	}
	if newerSemverCompatibleBD(bdVersion, libraryVersion) {
		return BDVersionNewer
	}
	if samePrereleaseSeries(bdVersion, libraryVersion) {
		return BDVersionSamePrerelease
	}
	bd := "v" + strings.TrimPrefix(strings.TrimSpace(bdVersion), "v")
	lib := "v" + strings.TrimPrefix(strings.TrimSpace(libraryVersion), "v")
	switch {
	case !semver.IsValid(bd) || !semver.IsValid(lib):
		return BDVersionUnparsed
	case semver.Compare(bd, lib) < 0:
		return BDVersionOlder
	default:
		// Not older, and not a newer release of the same major version.
		return BDVersionNewerMajor
	}
}
