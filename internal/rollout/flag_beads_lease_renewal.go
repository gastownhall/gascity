package rollout

import "github.com/gastownhall/gascity/internal/config"

// KeyBeadsLeaseRenewal is the exported registry Key for the claim-lease
// renewal rollout gate, so composition-root code (cmd/gc) can reference the
// gate without re-hardcoding the dotted string or matching it back out of the
// registry by a coincidental axis. keyBeadsLeaseRenewal is the package-internal
// spelling used throughout the resolver and registry.
const KeyBeadsLeaseRenewal = "beads.lease_renewal"

const keyBeadsLeaseRenewal = KeyBeadsLeaseRenewal

// envBeadsLeaseRenewal is the single source of truth for this gate's env
// override name: the registry Spec.EnvOverride, the resolver, and the
// testenv.LeakVectorVars membership test all reference it, so the three can
// never drift into a silent break-glass no-op.
const envBeadsLeaseRenewal = "GC_BEADS_LEASE_RENEWAL"

// BeadsLeaseRenewal returns the resolved beads.lease_renewal mode.
func (f Flags) BeadsLeaseRenewal() Mode {
	return f.beadsLeaseRenewal.value
}

// WithBeadsLeaseRenewal overrides beads.lease_renewal on a ForTest Flags value.
func WithBeadsLeaseRenewal(m Mode) ForTestOption {
	return func(b *flagsBuilder) {
		b.flags.beadsLeaseRenewal = resolved[Mode]{value: m, origin: OriginConfig}
	}
}

// readBeadsLeaseRenewal returns the raw config spelling for the gate and
// whether the merged config set it (empty string = unset, since the field is
// omitempty).
func readBeadsLeaseRenewal(cfg *config.City) (raw string, defined bool) {
	raw = cfg.Beads.LeaseRenewal
	return raw, raw != ""
}
