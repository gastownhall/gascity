package targetscope

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// BeadGetter is the one store capability the inherited walk needs. Narrowing it
// here keeps the readers (close gate, Ralph) free to pass whatever store class
// wrapper they already hold.
type BeadGetter interface {
	Get(id string) (beads.Bead, error)
}

// ResolveInherited finds the scope governing bead by walking bead → ParentID →
// gc.root_bead_id, mirroring dispatch.resolveInheritedMetadata.
//
// The walk exists because formula-generated stages do NOT carry a copy of root
// metadata — they carry a symbolic root reference — so a bead-local read would
// miss precisely the uncovered class this object was built for.
//
// The FIRST reachable non-empty value decides, and it decides in all three
// directions: if that value fails to parse, the answer is INVALID, not "keep
// walking" and not absent. Continuing past a malformed object would let a
// corrupt near value be silently replaced by a valid far one; degrading it to
// absent would re-enable the cwd writers. Absent is returned only when the walk
// completes having seen no value at all.
//
// A store error mid-walk is likewise INVALID, never absent: "I could not read
// the scope" must never be permission to stamp cwd.
func ResolveInherited(store BeadGetter, bead beads.Bead) Resolution {
	if raw := bead.Metadata[beadmeta.TargetScopeMetadataKey]; raw != "" {
		return Parse(raw)
	}
	if store == nil {
		return Resolution{State: StateAbsent}
	}
	current := bead
	visited := map[string]struct{}{}
	for {
		next, err := inheritedNext(store, current, visited)
		if err != nil {
			return Resolution{
				State:  StateInvalid,
				Reason: fmt.Errorf("%w: reading inherited scope from %s: %v", ErrInvalidScope, current.ID, err),
			}
		}
		if next == nil {
			return Resolution{State: StateAbsent}
		}
		current = *next
		if raw := current.Metadata[beadmeta.TargetScopeMetadataKey]; raw != "" {
			return Parse(raw)
		}
	}
}

// inheritedNext advances one hop along the parent chain then the root chain,
// returning nil when the walk is exhausted.
//
// A Get failure is reported rather than swallowed. dispatch's flat-key walk
// treats an unreadable ancestor as "no value here, carry on", which is right
// for a best-effort string but wrong for a guard: a transient store error would
// silently become "unscoped" and unlock the cwd stamp.
func inheritedNext(store BeadGetter, current beads.Bead, visited map[string]struct{}) (*beads.Bead, error) {
	for _, candidate := range []string{current.ParentID, current.Metadata[beadmeta.RootBeadIDMetadataKey]} {
		if candidate == "" || candidate == current.ID {
			continue
		}
		if _, seen := visited[candidate]; seen {
			continue
		}
		visited[candidate] = struct{}{}
		next, err := store.Get(candidate)
		if err != nil {
			return nil, err
		}
		return &next, nil
	}
	return nil, nil
}

// Envelope is the containment policy applied to a scope worktree before it is
// handed to git. It mirrors Ralph's existing rule: the path must sit under the
// city root or the store root.
type Envelope struct {
	CityPath  string
	StorePath string
}

// Contains reports whether path sits inside the envelope. An envelope with no
// roots configured contains nothing — an unconfigured envelope must fail closed
// rather than wave everything through.
func (e Envelope) Contains(path string) bool {
	if path == "" {
		return false
	}
	if e.CityPath != "" && pathutil.PathWithin(e.CityPath, path) {
		return true
	}
	if e.StorePath != "" && pathutil.PathWithin(e.StorePath, path) {
		return true
	}
	return false
}

// CheckWorktree validates a resolved scope's worktree for a reader.
//
// Parse has already rejected relative and uncleaned values; this adds the
// envelope check that keeps `git -C` from being pointed at a repo outside the
// city. Inherited metadata is mutable, so readers re-check rather than trusting
// that the boundary got it right.
//
// A scope with no worktree passes: unknown is a legitimate value and the caller
// decides what to do without one.
func (e Envelope) CheckWorktree(scope Scope) error {
	if scope.Worktree == "" {
		return nil
	}
	if !e.Contains(scope.Worktree) {
		return fmt.Errorf("%w: worktree %q escapes both city and store roots", ErrInvalidScope, scope.Worktree)
	}
	return nil
}

// ResolveForReader is the one call a behavior-critical consumer makes.
//
// It performs the inherited walk and then applies envelope containment,
// folding an escaping worktree into StateInvalid so a reader has exactly one
// tri-state to branch on. Readers must treat StateInvalid as a failure of the
// scoped read — never as license to fall back to the flat gc.work_* keys.
func ResolveForReader(store BeadGetter, bead beads.Bead, envelope Envelope) Resolution {
	res := ResolveInherited(store, bead)
	if res.State != StateValid {
		return res
	}
	if err := envelope.CheckWorktree(res.Scope); err != nil {
		return Resolution{State: StateInvalid, Reason: err, Raw: res.Raw}
	}
	return res
}
