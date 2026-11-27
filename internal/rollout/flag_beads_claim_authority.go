package rollout

import "github.com/gastownhall/gascity/internal/config"

// KeyBeadsClaimAuthority is the exported registry Key for the beads
// claim-authority rollout gate, so composition-root code (cmd/gc) can
// reference the gate without re-hardcoding the dotted string.
const KeyBeadsClaimAuthority = "beads.claim_authority"

const keyBeadsClaimAuthority = KeyBeadsClaimAuthority

// envBeadsClaimAuthority is the single source of truth for this gate's env
// override name: the registry Spec.EnvOverride, the resolver, and the
// testenv.LeakVectorVars membership test all reference it.
const envBeadsClaimAuthority = "GC_BEADS_CLAIM_AUTHORITY"

// BeadsClaimAuthority returns the resolved beads.claim_authority mode.
func (f Flags) BeadsClaimAuthority() Mode {
	return f.beadsClaimAuthority.value
}

// WithBeadsClaimAuthority overrides beads.claim_authority on a ForTest Flags
// value.
func WithBeadsClaimAuthority(m Mode) ForTestOption {
	return func(b *flagsBuilder) {
		b.flags.beadsClaimAuthority = resolved[Mode]{value: m, origin: OriginConfig}
	}
}

// readBeadsClaimAuthority returns the raw config spelling for the gate and
// whether the merged config set it (empty string = unset).
func readBeadsClaimAuthority(cfg *config.City) (raw string, defined bool) {
	raw = cfg.Beads.ClaimAuthority
	return raw, raw != ""
}
