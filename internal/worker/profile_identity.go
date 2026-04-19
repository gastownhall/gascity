package worker

import workerbuiltin "github.com/gastownhall/gascity/internal/worker/builtin"

// ProfileIdentity captures the explicit production identity for a canonical
// worker profile.
type ProfileIdentity = workerbuiltin.ProfileIdentity

// CanonicalProfileIdentity returns the compatibility identity for one of the
// canonical worker profiles.
func CanonicalProfileIdentity(profile Profile) (ProfileIdentity, bool) {
	return workerbuiltin.CanonicalProfileIdentity(string(profile))
}
