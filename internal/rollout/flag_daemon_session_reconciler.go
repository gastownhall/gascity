package rollout

import "github.com/gastownhall/gascity/internal/config"

// KeyDaemonSessionReconciler is the registry key for boot-latched keyed
// session-reconciler ownership.
const KeyDaemonSessionReconciler = "daemon.session_reconciler"

const keyDaemonSessionReconciler = KeyDaemonSessionReconciler

// SessionReconciler returns the resolved daemon.session_reconciler mode.
func (f Flags) SessionReconciler() Mode {
	return f.sessionReconciler.value
}

// WithSessionReconciler overrides daemon.session_reconciler on a ForTest Flags
// value.
func WithSessionReconciler(mode Mode) ForTestOption {
	return func(b *flagsBuilder) {
		b.flags.sessionReconciler = resolved[Mode]{value: mode, origin: OriginConfig}
	}
}

func readDaemonSessionReconciler(cfg *config.City) (raw string, defined bool) {
	raw = cfg.Daemon.SessionReconciler
	return raw, raw != ""
}
