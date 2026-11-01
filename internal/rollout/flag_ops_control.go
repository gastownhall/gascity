package rollout

import "github.com/gastownhall/gascity/internal/config"

// KeyOpsControl is the exported registry Key for the ops.control rollout gate:
// whether interrupt and respond requests are recorded as operation-journal ops beside their legacy
// execution (auto = shadow, require = durable).
const KeyOpsControl = "ops.control"

const keyOpsControl = KeyOpsControl

// envOpsControl is the single source of truth for this gate's env override
// name: the registry Spec.EnvOverride, the resolver, and the
// testenv.LeakVectorVars membership test all reference it.
const envOpsControl = "GC_OPS_CONTROL"

// OpsControl returns the resolved ops.control mode.
func (f Flags) OpsControl() Mode {
	return f.opsControl.value
}

// WithOpsControl overrides ops.control on a ForTest Flags value.
func WithOpsControl(m Mode) ForTestOption {
	return func(b *flagsBuilder) {
		b.flags.opsControl = resolved[Mode]{value: m, origin: OriginConfig}
	}
}

// readOpsControl returns the raw config spelling for the gate and whether the
// merged config set it (empty string = unset, since the field is omitempty).
func readOpsControl(cfg *config.City) (raw string, defined bool) {
	raw = cfg.Ops.ControlMode
	return raw, raw != ""
}

// opsControlSpec returns the canonical Spec for the ops.control gate.
func opsControlSpec() Spec { return specByKey(keyOpsControl) }
