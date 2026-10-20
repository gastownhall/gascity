package rollout

import "github.com/gastownhall/gascity/internal/config"

// KeyOpsLifecycle is the exported registry Key for the ops.lifecycle rollout gate:
// whether wake, suspend, restart, kill and close requests are recorded as operation-journal ops beside their legacy
// execution (auto = shadow, require = durable).
const KeyOpsLifecycle = "ops.lifecycle"

const keyOpsLifecycle = KeyOpsLifecycle

// envOpsLifecycle is the single source of truth for this gate's env override
// name: the registry Spec.EnvOverride, the resolver, and the
// testenv.LeakVectorVars membership test all reference it.
const envOpsLifecycle = "GC_OPS_LIFECYCLE"

// OpsLifecycle returns the resolved ops.lifecycle mode.
func (f Flags) OpsLifecycle() Mode {
	return f.opsLifecycle.value
}

// WithOpsLifecycle overrides ops.lifecycle on a ForTest Flags value.
func WithOpsLifecycle(m Mode) ForTestOption {
	return func(b *flagsBuilder) {
		b.flags.opsLifecycle = resolved[Mode]{value: m, origin: OriginConfig}
	}
}

// readOpsLifecycle returns the raw config spelling for the gate and whether the
// merged config set it (empty string = unset, since the field is omitempty).
func readOpsLifecycle(cfg *config.City) (raw string, defined bool) {
	raw = cfg.Ops.LifecycleMode
	return raw, raw != ""
}

// opsLifecycleSpec returns the canonical Spec for the ops.lifecycle gate.
func opsLifecycleSpec() Spec { return specByKey(keyOpsLifecycle) }
