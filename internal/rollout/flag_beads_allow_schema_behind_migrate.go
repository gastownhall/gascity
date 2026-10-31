package rollout

import "github.com/gastownhall/gascity/internal/config"

// KeyBeadsAllowSchemaBehindMigrate is the canonical dotted name for the
// beads.allow_schema_behind_migrate gate.
const KeyBeadsAllowSchemaBehindMigrate = "beads.allow_schema_behind_migrate"

const keyBeadsAllowSchemaBehindMigrate = KeyBeadsAllowSchemaBehindMigrate

// envBeadsAllowSchemaBehindMigrate is the registered break-glass override.
// This is the ONLY declaration of this env var name in the repo — the
// native-store preflight schema gate and the direct native-open path both
// read the gate's resolved value (Flags.AllowSchemaBehindMigrate), never a
// copy of this name or an ad-hoc os.Getenv/LookupEnv call.
const envBeadsAllowSchemaBehindMigrate = "GC_BEADS_ALLOW_SCHEMA_BEHIND_MIGRATE"

// AllowSchemaBehindMigrate reports whether the city has opted in to letting
// the linked beads library migrate its database forward when the database's
// own schema cursor trails the library's ceiling. Off (false) by default: a
// behind schema FAILs the native-store preflight and the direct native-open
// path withholds BD_ALLOW_REMOTE_MIGRATE from the linked library.
func (f Flags) AllowSchemaBehindMigrate() bool { return f.allowSchemaBehindMigrate.value }

// WithAllowSchemaBehindMigrate overrides the gate for ForTest.
func WithAllowSchemaBehindMigrate(v bool) ForTestOption {
	return func(b *flagsBuilder) {
		b.flags.allowSchemaBehindMigrate = resolved[bool]{value: v, origin: OriginConfig}
	}
}

// readBeadsAllowSchemaBehindMigrate reads cfg.Beads.AllowSchemaBehindMigrate.
// A nil pointer (the field omitted) is undefined, so Resolve falls through to
// the builtin default (false) unless the registered env override supplies a
// value.
func readBeadsAllowSchemaBehindMigrate(cfg *config.City) (value bool, defined bool) {
	if cfg.Beads.AllowSchemaBehindMigrate == nil {
		return false, false
	}
	return *cfg.Beads.AllowSchemaBehindMigrate, true
}
