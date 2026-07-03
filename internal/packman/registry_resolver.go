package packman

// VersionResolver resolves a version constraint against an authoritative
// external release table (such as a pack registry's release→commit mapping)
// instead of against the source repository's git tags.
//
// It returns ok=false when source is not known to any catalog, signaling the
// caller to fall back to git-tag resolution (preserving behavior for plain
// git-backed sources). A non-nil error is fatal and must propagate
// (fail-closed): it means source IS catalog-known but no published release
// satisfies constraint, or the catalog could not be consulted. The resolver
// must never silently fall back to git tags for a source the registry owns —
// that is the bug this seam exists to prevent.
type VersionResolver func(source, constraint string) (resolved ResolvedVersion, ok bool, err error)

// registryResolver is the catalog-backed resolver consulted before git-tag
// resolution. It is nil by default so packman stays registry-blind: with no
// resolver installed, ResolveVersion uses git tags exactly as before. It is
// expected to be installed exactly once at process startup (before any
// SyncLock call) and read-only thereafter, which makes the read in
// resolveSource race-free without synchronization. Tests save and restore it.
var registryResolver VersionResolver

// SetVersionResolver installs the registry-aware resolver that SyncLock
// consults before git-tag resolution. Passing nil clears it, restoring
// tag-only behavior; tests use that to reset package state.
func SetVersionResolver(r VersionResolver) {
	registryResolver = r
}
