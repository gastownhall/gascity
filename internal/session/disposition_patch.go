package session

import (
	"fmt"
	"maps"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// DispositionPatch is a disposition's class keys as Disposition.Encode or
// Changed produced them: the only form in which those keys may reach a
// session row. Into is the one way to put it in a write; in a guarded test
// the store's write chokepoint fails any class key no Into minted
// (GuardDispositionWrites), so a hand-built hold, intent or stamp fails the
// test that exercises it, whoever wrote it.
type DispositionPatch struct{ kv MetadataPatch }

// Get is key's value in p, "" when p does not carry it.
func (p DispositionPatch) Get(key string) string { return p.kv[key] }

// Has reports whether p carries key.
func (p DispositionPatch) Has(key string) bool {
	_, ok := p.kv[key]
	return ok
}

// Len is the number of keys p carries.
func (p DispositionPatch) Len() int { return len(p.kv) }

// Into adds p to patch, the write it rides to row id, and returns patch (a
// new one when patch is nil). It mints p for id: a guarded store write to id
// may carry exactly p's class keys.
func (p DispositionPatch) Into(id string, patch MetadataPatch) MetadataPatch {
	if patch == nil {
		patch = make(MetadataPatch, len(p.kv))
	}
	maps.Copy(patch, p.kv)
	mintDisposition(id, p.kv)
	return patch
}

// Fold is p as a plain patch for a caller's in-memory fold or read. It mints
// nothing: written, it fails the guard.
func (p DispositionPatch) Fold() MetadataPatch { return maps.Clone(p.kv) }

// Cell is the class keys of the move of meta's disposition by ev from by
// (its cell), meta being the row the write is decided on; empty when the
// cell refuses: the hold stands. It mints nothing, so a pure decider may
// call it.
func Cell(meta map[string]string, by ActorKind, ev DispEvent, now time.Time) DispositionPatch {
	to, err := DecodeDisposition(meta).Apply(by, ev, State(strings.TrimSpace(meta["state"])), now)
	if err != nil {
		return DispositionPatch{}
	}
	return to.Changed()
}

// CellInfo is Cell over a typed row.
func CellInfo(info Info, by ActorKind, ev DispEvent, now time.Time) DispositionPatch {
	return Cell(dispositionMeta(info), by, ev, now)
}

// CellInto adds Cell's class keys to patch, the write it rides to row id, and
// returns patch (a new one when patch is nil).
func CellInto(id string, meta map[string]string, by ActorKind, ev DispEvent, now time.Time, patch MetadataPatch) MetadataPatch {
	return Cell(meta, by, ev, now).Into(id, patch)
}

// CellIntoInfo is CellInto over a typed row.
func CellIntoInfo(info Info, by ActorKind, ev DispEvent, now time.Time, patch MetadataPatch) MetadataPatch {
	return CellInfo(info, by, ev, now).Into(info.ID, patch)
}

// UndoDisposition is the undo of a cell's write: for each class key written
// (the write's patch) that current (the row now) still holds as written,
// the value before held (the row the write was decided on). An undo restores
// only the row's own earlier text, never a value of the caller's.
func UndoDisposition(before, written, current map[string]string) DispositionPatch {
	undo := MetadataPatch{}
	for _, k := range dispositionKeys {
		if v, ok := written[k]; ok && current[k] == v && before[k] != v {
			undo[k] = before[k]
		}
	}
	return DispositionPatch{undo}
}

// dispositionGuard is the write guard's state in a guarded test: by row, the
// class writes Into minted that no store write has spent yet. A mint a lost
// or refused write never spends stays open; it admits only a later write to
// the same row of exactly its keys and values.
var dispositionGuard struct {
	on    atomic.Bool
	mu    sync.Mutex
	mints map[string][]MetadataPatch
}

func mintDisposition(id string, kv MetadataPatch) {
	if !dispositionGuard.on.Load() || len(kv) == 0 {
		return
	}
	dispositionGuard.mu.Lock()
	defer dispositionGuard.mu.Unlock()
	if dispositionGuard.mints == nil {
		dispositionGuard.mints = map[string][]MetadataPatch{}
	}
	dispositionGuard.mints[id] = append(dispositionGuard.mints[id], maps.Clone(kv))
}

// UnmintDisposition spends the mint a write to row id of written (the patch
// whose class keys a DispositionPatch.Into minted) left, when that write did
// not land: a lost CAS attempt, a refused or failed write. A writer that
// re-decides after a lost attempt mints again, so an unspent mint would
// otherwise admit a later hand-built write of the same content to any row of
// that ID, in any store of the test.
func UnmintDisposition(id string, written map[string]string) {
	if !dispositionGuard.on.Load() {
		return
	}
	class := MetadataPatch{}
	for _, k := range dispositionKeys {
		if v, ok := written[k]; ok {
			class[k] = v
		}
	}
	if len(class) > 0 {
		spendDisposition(id, class)
	}
}

// OpenDispositionMints is the number of mints for row id no write has spent:
// a guarded test's check that a writer unmints what did not land.
func OpenDispositionMints(id string) int {
	dispositionGuard.mu.Lock()
	defer dispositionGuard.mu.Unlock()
	return len(dispositionGuard.mints[id])
}

// spendDisposition reports whether class, a write's class keys to row id, is
// a mint for id, and spends it.
func spendDisposition(id string, class MetadataPatch) bool {
	dispositionGuard.mu.Lock()
	defer dispositionGuard.mu.Unlock()
	mints := dispositionGuard.mints[id]
	for i, m := range mints {
		if maps.Equal(m, class) {
			dispositionGuard.mints[id] = append(mints[:i], mints[i+1:]...)
			return true
		}
	}
	return false
}

// shape is a set of value forms a bypass writer may write to a class key.
type shape uint8

const (
	shapeEmpty shape       = 1 << iota // "": a clear
	shapeTime                          // an RFC3339 timestamp
	shapeAny   = ^shape(0)             // any text: an undo restores what its cell replaced, readable or not
)

func (s shape) admits(v string) bool {
	switch {
	case s == shapeAny:
		return true
	case v == "":
		return s&shapeEmpty != 0
	}
	_, err := time.Parse(time.RFC3339, v)
	return s&shapeTime != 0 && err == nil
}

// bypassWrite bounds a listed bypass writer: the class keys it writes today,
// each with the value forms it writes, and the cell that replaces it.
type bypassWrite struct {
	cell string
	keys map[string]shape
}

func (b bypassWrite) admits(class MetadataPatch) bool {
	for k, v := range class {
		if !b.keys[k].admits(v) {
			return false
		}
	}
	return true
}

// dispositionBypass are the production writers that still write class keys
// without a mint, by function, each bounded by the keys and value forms it
// writes and naming the cells it carries. The guard admits a write that fits
// the bound of a listed writer on its stack. Only v2's casRow is left: its
// deciders are pure and cannot mint, so their cells reach it folded until
// R7c2b carries them typed and deletes the list.
var dispositionBypass = map[string]bypassWrite{
	"cmd/gc.fencedWriter.closeRow": {"v2's A10 rollback close: the Retire cell, folded by its pure decider; R7c2b carries it typed and deletes this list", map[string]shape{"held_until": shapeEmpty, "sleep_intent": shapeEmpty, "wait_hold": shapeEmpty, "suspended_at": shapeEmpty, "quarantined_until": shapeEmpty}},
	"cmd/gc.fencedWriter.casRow":   {"v2 row patches (the launch's PreWake, the expired-timer TimerTick, the dead runtime's Sleep and quarantine, the stability clear): their cells, folded by the pure deciders; R7c2b carries them typed to the write and deletes this list", map[string]shape{"held_until": shapeEmpty, "sleep_intent": shapeEmpty, "wait_hold": shapeEmpty, "suspended_at": shapeEmpty, "quarantined_until": shapeEmpty | shapeTime}},
}

// dispositionPlumbing are the store wrappers between a writer and the backend,
// by file, and dispositionVerbs the session store's generic verbs, by
// function, which forward their caller's patch: the guard looks past both for
// the writer.
var (
	dispositionPlumbing = []string{"/internal/beads/", "/internal/session/disposition_patch.go", "/cmd/gc/class_store_emit.go", "/cmd/gc/bead_policy_store.go"}
	dispositionVerbs    = map[string]bool{
		"internal/session.(*Store).ApplyPatch": true, "internal/session.(*Store).ApplyPatchInfo": true,
		"internal/session.(*Store).commitIf": true, "internal/session.(*Store).applyPatchIfLifecycleUnchanged": true,
		"internal/session.(*Store).ApplyPatchIfLifecycleUnchanged": true, "internal/session.(*Store).ApplyPatchIfLifecycleUnchangedUnder": true,
		"internal/session.(*Store).UpdateMetadataFenced": true, "internal/session.(*Store).WithPendingCreateRollback": true,
		"internal/session.(*Store).CloseWithTerminalPatch": true, "internal/session.(*Store).CloseWithTerminalPatchUnder": true,
		"internal/session.(*Store).closeAtomically": true, "internal/session.(*Store).RollbackPendingCreateAtomically": true,
		"internal/session.(*Store).RollbackPendingCreateAtomicallyUnder": true, "internal/session.(*Store).CommitStartedIfCurrent": true,
		"internal/session.(*Store).CommitStartedIfCurrentUnder": true, "internal/session.(*Store).setMetadataValue": true,
		"internal/session.(*Store).SetMarker": true,
	}
)

// GuardDispositionWrites makes every store write of a session row (an update
// or a create's seed) that carries a disposition class key no
// DispositionPatch.Into minted for that row call fail with the keys and the
// stack. A write a _test.go file makes (through the plumbing at most) is a
// fixture and passes; so does one a listed bypass writer on the stack makes
// within its bound. Tests install it beside GuardSessionKeys; production
// never does.
func GuardDispositionWrites(fail func(msg string)) {
	dispositionGuard.on.Store(true)
	beads.SetSessionWriteHook(func(id string, kvs map[string]string) {
		if msg := checkDispositionWrite(id, kvs, dispositionWriter); msg != "" {
			keyViolations.Lock()
			keyViolations.msgs = append(keyViolations.msgs, msg)
			keyViolations.Unlock()
			fail(msg)
		}
	})
}

// checkDispositionWrite is the guard's verdict on one store write's metadata
// to row id: "" when it carries no class key, spends a mint for id, or writer
// (the stack's reading) finds a fixture or a listed bypass writer whose bound
// admits the class keys; else the violation.
func checkDispositionWrite(id string, kvs map[string]string, writer func() (at string, bypass []string, fixture bool)) string {
	class := MetadataPatch{}
	for _, k := range dispositionKeys {
		if v, ok := kvs[k]; ok {
			class[k] = v
		}
	}
	if len(class) == 0 || spendDisposition(id, class) {
		return ""
	}
	at, bypass, fixture := writer()
	if fixture {
		return ""
	}
	for _, name := range bypass {
		if dispositionBypass[name].admits(class) {
			return ""
		}
	}
	return fmt.Sprintf("session row %s disposition keys %v written by hand at %s (bypass writers on the stack, none admitting it: %v): write them through a cell (DispositionPatch.Into from Disposition.Changed)\n%s", id, class, at, bypass, stackText())
}

// dispositionWriter reads the stack of a store write: at is its first frames
// past the plumbing, innermost first; fixture when the first is a _test.go
// file's; bypass the listed bypass writers on the stack.
func dispositionWriter() (at string, bypass []string, fixture bool) {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(3, pcs)])
	for {
		f, more := frames.Next()
		name := dispositionFuncName(f.Function)
		if !plumbing(f.File) && !dispositionVerbs[name] {
			switch {
			case at == "":
				at = name
				if strings.HasSuffix(f.File, "_test.go") {
					return at, nil, true
				}
			case strings.Count(at, " <- ") < 3:
				at += " <- " + name
			}
			if _, ok := dispositionBypass[name]; ok {
				bypass = append(bypass, name)
			}
		}
		if !more {
			return at, bypass, false
		}
	}
}

// dispositionModule prefixes every function the bypass list names.
const dispositionModule = "github.com/gastownhall/gascity/"

func plumbing(file string) bool {
	if strings.HasSuffix(file, "_test.go") {
		return false
	}
	for _, p := range dispositionPlumbing {
		// An absolute path under go test, workspace-relative under bazel.
		if strings.Contains(file, p) || strings.HasPrefix(file, p[1:]) {
			return true
		}
	}
	return false
}

// dispositionFuncName is a frame's function as the bypass list names it: by
// its package's path in the module, closures folded into the function that
// holds them ("cmd/gc.reopenNamed.func1" is "cmd/gc.reopenNamed").
func dispositionFuncName(full string) string {
	name := full
	if i := strings.Index(full, dispositionModule); i >= 0 {
		name = full[i+len(dispositionModule):]
	}
	for {
		i := strings.LastIndex(name, ".func")
		if i < 0 {
			return name
		}
		name = name[:i]
	}
}

func stackText() string {
	buf := make([]byte, 16<<10)
	return string(buf[:runtime.Stack(buf, false)])
}
