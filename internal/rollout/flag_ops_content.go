package rollout

import "github.com/gastownhall/gascity/internal/config"

// KeyOpsContent is the exported registry Key for the ops.content rollout gate:
// whether message deliveries (submit, messages, nudge, mail notify) are recorded as operation-journal ops beside their legacy
// execution (auto = shadow, require = durable).
const KeyOpsContent = "ops.content"

const keyOpsContent = KeyOpsContent

// envOpsContent is the single source of truth for this gate's env override
// name: the registry Spec.EnvOverride, the resolver, and the
// testenv.LeakVectorVars membership test all reference it.
const envOpsContent = "GC_OPS_CONTENT"

// OpsContent returns the resolved ops.content mode.
func (f Flags) OpsContent() Mode {
	return f.opsContent.value
}

// WithOpsContent overrides ops.content on a ForTest Flags value.
func WithOpsContent(m Mode) ForTestOption {
	return func(b *flagsBuilder) {
		b.flags.opsContent = resolved[Mode]{value: m, origin: OriginConfig}
	}
}

// readOpsContent returns the raw config spelling for the gate and whether the
// merged config set it (empty string = unset, since the field is omitempty).
func readOpsContent(cfg *config.City) (raw string, defined bool) {
	raw = cfg.Ops.ContentMode
	return raw, raw != ""
}

// opsContentSpec returns the canonical Spec for the ops.content gate.
func opsContentSpec() Spec { return specByKey(keyOpsContent) }
