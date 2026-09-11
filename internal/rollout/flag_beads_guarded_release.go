package rollout

import "github.com/gastownhall/gascity/internal/config"

// KeyBeadsGuardedRelease is the exported registry Key for the beads
// guarded-release rollout gate, so composition-root code (cmd/gc, internal/api)
// can reference the gate without re-hardcoding the dotted string or matching it
// back out of the registry by a coincidental axis. keyBeadsGuardedRelease is
// the package-internal spelling used throughout the resolver and registry.
const KeyBeadsGuardedRelease = "beads.guarded_release"

const keyBeadsGuardedRelease = KeyBeadsGuardedRelease

// envBeadsGuardedRelease is the single source of truth for this gate's env
// override name: the registry Spec.EnvOverride, the resolver, and the
// testenv.LeakVectorVars membership test all reference it, so the three can
// never drift into a silent break-glass no-op.
const envBeadsGuardedRelease = "GC_BEADS_GUARDED_RELEASE"

// BeadsGuardedRelease returns the resolved beads.guarded_release mode.
func (f Flags) BeadsGuardedRelease() Mode {
	return f.beadsGuardedRelease.value
}

// WithBeadsGuardedRelease overrides beads.guarded_release on a ForTest Flags
// value.
func WithBeadsGuardedRelease(m Mode) ForTestOption {
	return func(b *flagsBuilder) {
		b.flags.beadsGuardedRelease = resolved[Mode]{value: m, origin: OriginConfig}
	}
}

// readBeadsGuardedRelease returns the raw config spelling for the gate and
// whether the merged config set it (empty string = unset, since the field is
// omitempty).
func readBeadsGuardedRelease(cfg *config.City) (raw string, defined bool) {
	raw = cfg.Beads.GuardedRelease
	return raw, raw != ""
}
