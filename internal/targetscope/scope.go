// Package targetscope owns gc.target_scope: the declared answer to "where does
// this bead's work live, and against what branch is it validated".
//
// The defect it exists to close: gc.work_branch and gc.work_dir are derived at
// CLAIM time (branch) and RECONCILE time (dir) from the claiming session's cwd,
// not from any declared intent. A pool session parked on a shared checkout
// stamps that parked branch and shared root onto whatever it claims, and every
// later claim from that cwd re-inherits the poison. Formula-generated stage
// beads are the uncovered class by construction: they carry no declared
// location at all, so no prompt-level or worker-level fallback can repair them.
//
// The fix is to attach a first-class scope object at INSTANTIATION, resolved
// only from trusted sources, made immutable once declared, and read directly by
// the behavior-critical consumers. Two properties carry the whole design:
//
// TRI-STATE, NOT BOOLEAN. Absence is not "unknown". Absence is the legacy
// state, and it is the ONLY state that re-enables the cwd writers and the flat
// gc.work_* reader fallbacks. "Unknown" is a valid object with the field
// missing — Scope{V:1} — which suppresses cwd forever after. Collapsing a parse
// or read failure to "not found" reopens the defect, so Parse reports invalid
// distinctly from absent and no caller may conflate them.
//
// WHOLE-SCOPE SEMANTICS. A valid scope suppresses the cwd fallback for the
// entire scope, not field by field. A Scope{V:1, Branch:X} cannot acquire a cwd
// worktree, and a Scope{V:1, Worktree:Y} cannot acquire a cwd branch. Filling
// one absent field from cwd is how a half-poisoned object would be born.
//
// Provenance is enforced by omission: nothing in this package accepts
// gc.work_branch, gc.work_dir, or gc.work_commit as an input. Those keys are
// claim-poisoned by the very writers this object exists to invert, and they
// carry no marker separating a declaration from a cwd stamp — so laundering
// them into a scope would make the poison immutable under the new guards.
package targetscope

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// SchemaVersion is the only supported value of Scope.V. A different version is
// present-invalid, never absent: an object written by a newer binary must fail
// the scoped read rather than silently degrade to cwd.
const SchemaVersion = 1

// Scope is the persisted target-scope object.
//
// v1 is deliberately {v, branch, worktree} and nothing else. base_sha, origin,
// and repo were dropped because no reader justifies them, and an unread field
// is a field that drifts. Worktree containment (rather than a repo identity
// field) is what keeps a scope from pointing outside the envelope.
//
// It persists as one JSON blob so it round-trips the inherited-metadata walk as
// a single value and cannot half-apply.
type Scope struct {
	V int `json:"v"`
	// Branch feeds the close gate's reachability base.
	Branch string `json:"branch,omitempty"`
	// Worktree feeds gc.work_dir and Ralph's script base. It is ALWAYS
	// absolute once persisted — normalization happens at the launch boundary
	// (NormalizeWorktree), never at read time. See the State docs.
	Worktree string `json:"worktree,omitempty"`
}

// State is the tri-state every reader and writer must branch on.
type State int

const (
	// StateAbsent means no gc.target_scope was reachable. This is the ONLY
	// state that permits a cwd stamp or a flat gc.work_* reader fallback.
	StateAbsent State = iota
	// StateValid means the object parsed and is schema-valid. It is
	// authoritative for the whole scope; a missing field means unknown and
	// must never be filled from cwd.
	StateValid
	// StateInvalid means the object was reachable but unusable: malformed
	// JSON, unsupported version, wrong field type, or a worktree that is
	// relative or escaping. Writers must not stamp cwd and readers must FAIL
	// the scoped read. Never falls back to flat keys.
	StateInvalid
)

func (s State) String() string {
	switch s {
	case StateAbsent:
		return "absent"
	case StateValid:
		return "valid"
	case StateInvalid:
		return "invalid"
	default:
		return fmt.Sprintf("state(%d)", int(s))
	}
}

// ErrInvalidScope is the sentinel wrapped by every present-invalid reason, so
// callers can distinguish "this object is unusable" (fail closed) from a
// transport error without string matching.
var ErrInvalidScope = errors.New("invalid gc.target_scope")

// Resolution is a parsed scope plus the state that produced it. Reason is
// non-nil exactly when State is StateInvalid.
type Resolution struct {
	State  State
	Scope  Scope
	Reason error
	// Raw is the exact stored string that produced this resolution. The
	// member-declaration protocol feeds it back as the CAS expected value, so
	// it must be the bytes read, not a re-marshaling.
	Raw string
}

// Valid reports whether the resolution is present-valid.
func (r Resolution) Valid() bool { return r.State == StateValid }

// Absent reports whether no object was reachable. Only this state unlocks the
// legacy cwd/flat behavior.
func (r Resolution) Absent() bool { return r.State == StateAbsent }

// Invalid reports whether an object was reachable but unusable.
func (r Resolution) Invalid() bool { return r.State == StateInvalid }

// Parse turns a stored metadata value into the tri-state.
//
// An empty or whitespace-only raw value is absent — that is the pre-existing
// population and the only input that may re-enable legacy behavior. Everything
// else that fails to yield a schema-valid object is INVALID, never absent.
func Parse(raw string) Resolution {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Resolution{State: StateAbsent, Raw: raw}
	}
	invalid := func(format string, args ...any) Resolution {
		return Resolution{
			State:  StateInvalid,
			Reason: fmt.Errorf("%w: %s", ErrInvalidScope, fmt.Sprintf(format, args...)),
			Raw:    raw,
		}
	}
	var scope Scope
	dec := json.NewDecoder(strings.NewReader(trimmed))
	if err := dec.Decode(&scope); err != nil {
		// Wrong JSON types land here too (a numeric "branch" fails to
		// unmarshal into string), which is the intended classification.
		return invalid("malformed JSON: %v", err)
	}
	if dec.More() {
		return invalid("trailing content after JSON object")
	}
	if scope.V != SchemaVersion {
		return invalid("unsupported version %d (want %d)", scope.V, SchemaVersion)
	}
	if strings.TrimSpace(scope.Branch) != scope.Branch {
		return invalid("branch has leading or trailing whitespace")
	}
	if wt := scope.Worktree; wt != "" {
		if strings.TrimSpace(wt) != wt {
			return invalid("worktree has leading or trailing whitespace")
		}
		// A persisted RELATIVE worktree is present-invalid, not something to
		// re-anchor. Boundaries are required to persist absolute paths
		// (NormalizeWorktree), and the root and the member may live in
		// DIFFERENT stores — so a reader that re-anchored a relative value
		// against its own store root would produce a different path than the
		// other reader. Failing here is what keeps one absolute value the sole
		// authority instead of letting each reader pick an anchor.
		if !filepath.IsAbs(wt) {
			return invalid("worktree %q is relative; boundaries must persist an absolute path", wt)
		}
		if filepath.Clean(wt) != wt {
			return invalid("worktree %q is not in cleaned form", wt)
		}
	}
	return Resolution{State: StateValid, Scope: scope, Raw: raw}
}

// Marshal renders a scope for persistence. It rejects a scope it would not
// itself parse back as valid, so a caller cannot write a blob that every reader
// will then fail closed on.
func Marshal(scope Scope) (string, error) {
	if scope.V == 0 {
		scope.V = SchemaVersion
	}
	if scope.V != SchemaVersion {
		return "", fmt.Errorf("targetscope: unsupported version %d", scope.V)
	}
	if wt := scope.Worktree; wt != "" && !filepath.IsAbs(wt) {
		return "", fmt.Errorf("targetscope: worktree %q must be absolute; normalize at the launch boundary", wt)
	}
	blob, err := json.Marshal(scope)
	if err != nil {
		return "", fmt.Errorf("targetscope: marshal: %w", err)
	}
	return string(blob), nil
}

// Unknown is the valid field-empty object.
//
// This is what a boundary stamps when no trusted source supplies a field. It is
// NOT the same as writing nothing: an absent object lets the next claim or
// reconcile stamp cwd, while Unknown suppresses that permanently. "Leave the
// field unknown" must never be implemented by omitting the object.
func Unknown() Scope { return Scope{V: SchemaVersion} }

// IsUnknown reports whether the scope declares no fields.
func (s Scope) IsUnknown() bool { return s.Branch == "" && s.Worktree == "" }

// Equal compares field for field. The member-declaration protocol treats an
// equal re-declaration as a no-op and any difference as a launch rejection, so
// this is the exact predicate that decides immutability.
func (s Scope) Equal(other Scope) bool {
	return s.V == other.V && s.Branch == other.Branch && s.Worktree == other.Worktree
}

// NormalizeWorktree produces the mandatory absolute form for a worktree value,
// anchored to the store root that owns the resolution point.
//
// This runs at the LAUNCH BOUNDARY, before the scope is copied to the root or
// to any member, and the single absolute result is what gets persisted. That
// ordering is the whole point: resolving once at the boundary and copying the
// identical string everywhere means the graph-store reader and the work-store
// reader consume one authority, instead of each joining a relative value to its
// own root and disagreeing.
//
// An empty worktree stays empty — unknown is a legitimate value and must not
// become the store root.
func NormalizeWorktree(worktree, storeRoot string) (string, error) {
	worktree = strings.TrimSpace(worktree)
	if worktree == "" {
		return "", nil
	}
	if filepath.IsAbs(worktree) {
		return filepath.Clean(worktree), nil
	}
	storeRoot = strings.TrimSpace(storeRoot)
	if storeRoot == "" {
		return "", fmt.Errorf("targetscope: cannot anchor relative worktree %q: no store root at the boundary", worktree)
	}
	if !filepath.IsAbs(storeRoot) {
		abs, err := filepath.Abs(storeRoot)
		if err != nil {
			return "", fmt.Errorf("targetscope: cannot absolutize store root %q: %w", storeRoot, err)
		}
		storeRoot = abs
	}
	return filepath.Clean(filepath.Join(storeRoot, worktree)), nil
}

// Normalize returns a copy of scope with its worktree normalized to absolute
// against storeRoot. Boundaries call this before persisting.
func Normalize(scope Scope, storeRoot string) (Scope, error) {
	abs, err := NormalizeWorktree(scope.Worktree, storeRoot)
	if err != nil {
		return Scope{}, err
	}
	if scope.V == 0 {
		scope.V = SchemaVersion
	}
	scope.Worktree = abs
	return scope, nil
}
