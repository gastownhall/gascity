package rollout

import (
	"fmt"
	"os"

	"github.com/gastownhall/gascity/internal/config"
)

// ResolveOptions carries the injected seams. The zero value is production
// behavior (os.LookupEnv). Tests inject a map-backed LookupEnv — never t.Setenv.
type ResolveOptions struct {
	// LookupEnv defaults to os.LookupEnv when nil. It is never read at package
	// init; it is consulted only inside Resolve.
	LookupEnv func(key string) (string, bool)
}

// Resolve computes the immutable Flags value once per process from the
// already-merged config plus env overrides. Precedence is built-in default <
// config < env (per each gate's EnvSemantics), with a typed Origin and typed
// Notices recorded ON the returned Flags.
//
// A malformed env value NEVER fails Resolve: it records a NoticeInvalidEnvIgnored
// and keeps the config-resolved value (warn-and-use-config, never
// refuse-to-start). The error return is reserved for structural failures only:
// a nil cfg, or an out-of-enum non-empty CONFIG value (a config typo can never
// silently mean "off").
func Resolve(cfg *config.City, opts ResolveOptions) (Flags, error) {
	if cfg == nil {
		return Flags{}, fmt.Errorf("rollout: Resolve requires a non-nil config")
	}
	lookup := opts.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}

	f := defaultFlags()

	// beads.conditional_writes — Mode gate, EnvOverrides semantics.
	if err := resolveModeGate(cfg, lookup, &f, beadsConditionalWritesSpec(),
		readBeadsConditionalWrites,
		func(f *Flags, r resolved[Mode]) { f.beadsConditionalWrites = r }); err != nil {
		return Flags{}, err
	}

	// beads.guarded_release — Mode gate, EnvOverrides semantics.
	if err := resolveModeGate(cfg, lookup, &f, beadsGuardedReleaseSpec(),
		readBeadsGuardedRelease,
		func(f *Flags, r resolved[Mode]) { f.beadsGuardedRelease = r }); err != nil {
		return Flags{}, err
	}

	// daemon.formula_v2 — bool migration gate, no env override.
	if value, defined := readDaemonFormulaV2(cfg); defined {
		f.formulaV2 = resolved[bool]{value: value, origin: OriginConfig}
	}

	return f, nil
}

// resolveModeGate resolves one Off/Auto/Require gate from config plus its
// registry-declared env override, recording a typed Origin and the precedence
// Notices on f. It is the shared body for every Mode gate, so all of them obey
// identical precedence and notice semantics; only the per-gate wiring — which
// config field to read and which Flags slot to write — is injected via
// read/set. The env var NAME and precedence semantics come from the passed
// Spec, so the CODEOWNERS-reviewed registry is the single source of truth:
// renaming Spec.EnvOverride or flipping EnvSemantics changes behavior here, and
// the registry↔resolver binding test proves it.
func resolveModeGate(
	cfg *config.City,
	lookup func(string) (string, bool),
	f *Flags,
	spec Spec,
	read func(*config.City) (raw string, defined bool),
	set func(*Flags, resolved[Mode]),
) error {
	key := spec.Key

	raw, defined := read(cfg)
	mode, origin := Off, OriginBuiltin
	if defined {
		m, err := ParseMode(raw)
		if err != nil {
			return fmt.Errorf("rollout: config %s: %w", key, err)
		}
		mode, origin = m, OriginConfig
	}

	if spec.EnvOverride != "" {
		if envRaw, ok := lookup(spec.EnvOverride); ok {
			m, err := ParseMode(envRaw)
			switch {
			case err != nil:
				// Malformed value: warn and keep the config-resolved value. Never
				// refuse-to-start, never a silent fallback.
				f.notices = append(f.notices, Notice{
					Kind: NoticeInvalidEnvIgnored, FlagKey: key,
					EnvVar: spec.EnvOverride, ConfigValue: raw, EnvValue: envRaw,
					Message: fmt.Sprintf("%s=%q is not off|auto|require; ignored, keeping %s=%q (%s)",
						spec.EnvOverride, envRaw, key, string(mode), origin),
				})
			case spec.EnvSemantics == EnvFillsNil && defined:
				// fills-nil: config already set, so the env value does not apply.
				// No override, no misleading notice.
			case defined && m != mode:
				f.notices = append(f.notices, Notice{
					Kind: NoticeEnvOverridesConfig, FlagKey: key,
					EnvVar: spec.EnvOverride, ConfigValue: raw, EnvValue: envRaw,
					Message: fmt.Sprintf("%s=%q overrides config %s=%q", spec.EnvOverride, string(m), key, raw),
				})
				mode, origin = m, OriginEnv
			case defined && m == mode:
				// Env agrees with an explicit config value: redundant, so keep the
				// config origin and emit no (misleading "config unset") notice.
			default: // !defined: env supplies the value.
				f.notices = append(f.notices, Notice{
					Kind: NoticeEnvOverrideActive, FlagKey: key,
					EnvVar: spec.EnvOverride, ConfigValue: raw, EnvValue: envRaw,
					Message: fmt.Sprintf("%s=%q applied (config unset)", spec.EnvOverride, string(m)),
				})
				mode, origin = m, OriginEnv
			}
		}
	}

	set(f, resolved[Mode]{value: mode, origin: origin})
	return nil
}
