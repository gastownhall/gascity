package rollout

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// Category classifies why a gate exists. It is a CLOSED enum with no
// agent-capability member — the structural half of the "no capability flags"
// exclusion. Rollout and migration gates are terminal (their default flips, then
// the gate is deleted); only a killswitch is long-lived.
type Category string

const (
	// InfraRollout adopts a new mechanical path (e.g. beads CAS writes).
	InfraRollout Category = "infra-rollout"
	// InfraMigration retires a legacy path (e.g. the formula_v2 migration).
	InfraMigration Category = "infra-migration"
	// InfraKillswitch is an emergency off with no expiry — the only long-lived
	// category.
	InfraKillswitch Category = "infra-killswitch"
)

// EnvSemantics pins how a Spec's env override interacts with explicit config.
type EnvSemantics string

const (
	// EnvOverrides makes a valid env value beat explicit config (break-glass;
	// the default for a new gate).
	EnvOverrides EnvSemantics = "overrides"
	// EnvFillsNil applies the env value only when config left the field unset.
	EnvFillsNil EnvSemantics = "fills-nil"
)

// Default carries the built-in value. Exactly one arm is set, and the set arm
// fixes the gate's value kind (Mode vs bool). Enforced by ValidateSpecs.
type Default struct {
	Mode *Mode
	Bool *bool
}

// Owner is dual: Bead tracks the work item; GitHub is the named human/team that
// CODEOWNERS review and the lifecycle radar can actually reach.
type Owner struct {
	Bead   string // e.g. "ga-xxxxx"
	GitHub string // "@handle" or "@org/team"
}

// Spec is one rollout-gate descriptor. It is PURE DATA — no func-valued fields
// (a capability predicate is supplied per-call, never stored here) — so
// registry.go stays CODEOWNERS-reviewable and graduation edits stay data-only.
type Spec struct {
	Key            string       // canonical dotted name, unique, non-empty
	Category       Category     // member of the closed enum
	ConfigPath     string       // toml path on config.City; reflection-verified
	EnvOverride    string       // "" or exactly one GC_*-prefixed var, unique
	EnvSemantics   EnvSemantics // meaningful only when EnvOverride != ""
	Default        Default
	Owner          Owner
	Expires        string    // YYYY-MM-DD; mandatory for rollout/migration, forbidden for killswitch
	VersionAnchor  string    // names a deps.env key (or in-repo anchor); mandatory for rollout/migration, forbidden for killswitch
	SelectsBetween [2]string // the two mechanical code paths, both non-empty and distinct
	Justification  string    // the written litmus answer; presence checked here, truth in review
}

// ValidateSpecs reports every structural violation across specs. It takes the
// registry as a PARAMETER and returns errors (never panics), so registry_test
// validates the canonical set while subsystem tests validate throwaway []Spec
// literals with zero shared state.
func ValidateSpecs(specs []Spec) []error {
	var errs []error
	seenKey := map[string]bool{}
	seenEnv := map[string]bool{}
	for _, s := range specs {
		id := s.Key
		if id == "" {
			errs = append(errs, fmt.Errorf("spec with empty Key: %+v", s))
			id = "<empty>"
		} else if seenKey[s.Key] {
			errs = append(errs, fmt.Errorf("duplicate Spec.Key %q", s.Key))
		}
		seenKey[s.Key] = true

		switch s.Category {
		case InfraRollout, InfraMigration, InfraKillswitch:
		default:
			errs = append(errs, fmt.Errorf("%s: invalid Category %q", id, s.Category))
		}

		// Exactly one Default arm, matched to the config field's kind.
		switch {
		case s.Default.Mode != nil && s.Default.Bool != nil:
			errs = append(errs, fmt.Errorf("%s: Default sets both Mode and Bool", id))
		case s.Default.Mode == nil && s.Default.Bool == nil:
			errs = append(errs, fmt.Errorf("%s: Default sets neither Mode nor Bool", id))
		}

		// ConfigPath must resolve against config.City and match the value kind.
		if s.ConfigPath == "" {
			errs = append(errs, fmt.Errorf("%s: empty ConfigPath", id))
		} else if ft, ok := configFieldType(s.ConfigPath); !ok {
			errs = append(errs, fmt.Errorf("%s: ConfigPath %q does not resolve to a config.City field", id, s.ConfigPath))
		} else if kerr := checkDefaultMatchesField(id, s.Default, ft); kerr != nil {
			errs = append(errs, kerr)
		}

		// Env override hygiene.
		if s.EnvOverride != "" {
			if !strings.HasPrefix(s.EnvOverride, "GC_") {
				errs = append(errs, fmt.Errorf("%s: EnvOverride %q must be GC_-prefixed", id, s.EnvOverride))
			}
			if seenEnv[s.EnvOverride] {
				errs = append(errs, fmt.Errorf("%s: duplicate EnvOverride %q", id, s.EnvOverride))
			}
			seenEnv[s.EnvOverride] = true
			switch s.EnvSemantics {
			case EnvOverrides, EnvFillsNil:
			default:
				errs = append(errs, fmt.Errorf("%s: EnvOverride set but EnvSemantics %q invalid", id, s.EnvSemantics))
			}
		}

		// Owner is always required.
		if s.Owner.Bead == "" || s.Owner.GitHub == "" {
			errs = append(errs, fmt.Errorf("%s: Owner requires both Bead and GitHub", id))
		}

		// Lifecycle anchors: mandatory for rollout/migration, forbidden for killswitch.
		terminal := s.Category == InfraRollout || s.Category == InfraMigration
		if terminal {
			if s.Expires == "" {
				errs = append(errs, fmt.Errorf("%s: %s gate requires Expires (YYYY-MM-DD)", id, s.Category))
			} else if !isYYYYMMDD(s.Expires) {
				errs = append(errs, fmt.Errorf("%s: Expires %q is not YYYY-MM-DD", id, s.Expires))
			}
			if s.VersionAnchor == "" {
				errs = append(errs, fmt.Errorf("%s: %s gate requires a VersionAnchor", id, s.Category))
			}
		} else { // killswitch
			if s.Expires != "" {
				errs = append(errs, fmt.Errorf("%s: killswitch must not set Expires", id))
			}
			if s.VersionAnchor != "" {
				errs = append(errs, fmt.Errorf("%s: killswitch must not set VersionAnchor", id))
			}
		}

		if s.SelectsBetween[0] == "" || s.SelectsBetween[1] == "" {
			errs = append(errs, fmt.Errorf("%s: SelectsBetween needs two non-empty paths", id))
		} else if s.SelectsBetween[0] == s.SelectsBetween[1] {
			errs = append(errs, fmt.Errorf("%s: SelectsBetween paths must differ", id))
		}

		if s.Justification == "" {
			errs = append(errs, fmt.Errorf("%s: empty Justification", id))
		}
	}
	return errs
}

// checkDefaultMatchesField verifies the Default arm agrees with the config
// field's kind: a Mode gate maps to a string field; a bool gate maps to a bool
// or *bool field.
func checkDefaultMatchesField(id string, d Default, ft reflect.Type) error {
	switch {
	case d.Mode != nil:
		if ft.Kind() != reflect.String {
			return fmt.Errorf("%s: Mode gate expects a string config field, got %s", id, ft.Kind())
		}
	case d.Bool != nil:
		k := ft.Kind()
		if k == reflect.Pointer {
			k = ft.Elem().Kind()
		}
		if k != reflect.Bool {
			return fmt.Errorf("%s: bool gate expects a bool/*bool config field, got %s", id, ft.Kind())
		}
	}
	return nil
}

// configFieldType walks config.City by dotted toml path and returns the type of
// the addressed field. Pointer-to-struct segments are dereferenced during the
// walk; the final field's own type (pointer included) is returned.
func configFieldType(path string) (reflect.Type, bool) {
	t := reflect.TypeOf(config.City{})
	segs := strings.Split(path, ".")
	for i, seg := range segs {
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return nil, false
		}
		f, ok := fieldByTOMLName(t, seg)
		if !ok {
			return nil, false
		}
		if i == len(segs)-1 {
			return f.Type, true
		}
		t = f.Type
	}
	return nil, false
}

// fieldByTOMLName finds the struct field whose toml tag name equals name.
func fieldByTOMLName(t reflect.Type, name string) (reflect.StructField, bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("toml")
		if tag == "" || tag == "-" {
			continue
		}
		if before, _, _ := strings.Cut(tag, ","); before == name {
			return f, true
		}
	}
	return reflect.StructField{}, false
}

// isYYYYMMDD reports whether s is a plausible YYYY-MM-DD date (shape only; not a
// calendar check — this is a lint of the field, not a merge-blocking clock).
func isYYYYMMDD(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for i, r := range s {
		if i == 4 || i == 7 {
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
