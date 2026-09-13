package rollout

import "github.com/gastownhall/gascity/internal/config"

// keyDaemonFormulaV2 is the registry Key for the formula_v2 migration gate.
const keyDaemonFormulaV2 = "daemon.formula_v2"

// FormulaV2 returns the resolved daemon.formula_v2 value (the kill-switch for the
// legacy formula v1 path; default true).
func (f Flags) FormulaV2() bool {
	return f.formulaV2.value
}

// WithFormulaV2 overrides daemon.formula_v2 on a ForTest Flags value.
func WithFormulaV2(enabled bool) ForTestOption {
	return func(b *flagsBuilder) {
		b.flags.formulaV2 = resolved[bool]{value: enabled, origin: OriginConfig}
	}
}

// readDaemonFormulaV2 reads cfg.Daemon.FormulaV2; a nil pointer means unset (the
// built-in default, true).
func readDaemonFormulaV2(cfg *config.City) (value bool, defined bool) {
	if cfg.Daemon.FormulaV2 == nil {
		return true, false
	}
	return *cfg.Daemon.FormulaV2, true
}
