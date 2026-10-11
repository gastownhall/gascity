package session

import (
	"fmt"
	"maps"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

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

// Into adds p to patch, the write it rides, and returns patch (a new one
// when patch is nil). It mints p for the guard: the store write of patch may
// carry exactly p's class keys.
func (p DispositionPatch) Into(patch MetadataPatch) MetadataPatch {
	if patch == nil {
		patch = make(MetadataPatch, len(p.kv))
	}
	maps.Copy(patch, p.kv)
	mintDisposition(p.kv)
	return patch
}

// Fold is p as a plain patch for a caller's in-memory fold or read. It mints
// nothing: written, it fails the guard.
func (p DispositionPatch) Fold() MetadataPatch { return maps.Clone(p.kv) }

// dispositionGuard is the write guard's state in a guarded test: the class
// writes Into minted that no store write has spent yet. A mint a lost or
// refused write never spends stays open; it admits only a write of exactly
// its keys and values.
var dispositionGuard struct {
	on    atomic.Bool
	mu    sync.Mutex
	mints []MetadataPatch
}

func mintDisposition(kv MetadataPatch) {
	if !dispositionGuard.on.Load() || len(kv) == 0 {
		return
	}
	dispositionGuard.mu.Lock()
	dispositionGuard.mints = append(dispositionGuard.mints, maps.Clone(kv))
	dispositionGuard.mu.Unlock()
}

// spendDisposition reports whether class, a write's class keys, is a mint,
// and spends it.
func spendDisposition(class MetadataPatch) bool {
	dispositionGuard.mu.Lock()
	defer dispositionGuard.mu.Unlock()
	for i, m := range dispositionGuard.mints {
		if maps.Equal(m, class) {
			dispositionGuard.mints = append(dispositionGuard.mints[:i], dispositionGuard.mints[i+1:]...)
			return true
		}
	}
	return false
}

// dispositionBypass are the production writers that still write class keys
// by hand, by function, each with the change that moves it onto a cell. The
// guard admits a write with one of them on its stack. The list only shrinks:
// R7c2 empties it.
var dispositionBypass = map[string]string{
	"cmd/gc.preWakeCommitWith":                                                 "PreWakePatch's sleep_intent: the PreWake cell",
	"cmd/gc.restorePreWakeState":                                               "the PreWake rollback: an undo of the cell's own write",
	"cmd/gc.fencedWriter.casRow":                                               "v2 row patches (the launch's PreWakePatch, the expired-timer clears, the heal's quarantine accrual and clear): their cells",
	"cmd/gc.closeFailedCreateBead":                                             "failedCreateClosePatch's sleep_intent: the Retire cell",
	"cmd/gc.closeFailedCreateBeadInTx":                                         "failedCreateClosePatch's sleep_intent: the Retire cell",
	"cmd/gc.rollbackPendingCreateClears":                                       "failedCreateClosePatch's sleep_intent: the Retire cell",
	"cmd/gc.reopenClosedConfiguredNamedSessionBead":                            "the named reopen's ClearWakeBlockersPatch: the Retire cell",
	"cmd/gc.reopenNamed":                                                       "the named reopen's ClearWakeBlockersPatch: the Retire cell",
	"cmd/gc.retireDuplicateConfiguredNamedSessionBeads":                        "RetireNamedSessionPatch: the Retire cell",
	"cmd/gc.retireDuplicateConfiguredNamedSessionRows":                         "RetireNamedSessionPatch: the Retire cell",
	"cmd/gc.retireRemovedConfiguredNamedSessionBead":                           "RetireNamedSessionPatch: the Retire cell",
	"internal/api.(*Server).retireContinuityIneligibleNamedSessionIdentifiers": "RetireNamedSessionPatch: the Retire cell",
	"cmd/gc.healExpiredTimersInfo":                                             "ClearExpiredHoldPatch, ClearExpiredQuarantinePatch: the TimerTick cell",
	"cmd/gc.clearWakeFailures":                                                 "the quarantine clear: the TimerTick cell",
	"cmd/gc.recordWakeFailure":                                                 "the wake-failure quarantine: the Quarantine cell",
	"cmd/gc.recordChurn":                                                       "the churn quarantine: the Quarantine cell",
	"cmd/gc.recordRateLimitQuarantine":                                         "the rate-limit quarantine: the Quarantine cell",
	"cmd/gc.(*sessionKillFence).rollback":                                      "the kill fence rollback: an undo of the Kill cell's own write",
	"internal/session.(*Manager).Suspend":                                      "the soft suspend's suspended_at and the failed-stop rollback: the Suspend cell and its undo",
	"internal/session.(*Manager).clearWakeAndHoldOverrides":                    "the close's hold clear: the Retire cell",
}

// dispositionPlumbing are the generic write verbs and store wrappers between a
// writer and the backend: the guard looks past them for the writer.
var dispositionPlumbing = []string{"/internal/beads/", "/internal/session/store.go", "/internal/session/disposition_patch.go", "/cmd/gc/class_store_emit.go"}

// GuardDispositionWrites makes every store update of a session row that
// carries a disposition class key no DispositionPatch.Into minted call fail
// with the keys and the stack. A write a _test.go file makes (through the
// generic verbs at most) is a fixture and passes; so does a listed bypass
// writer's. Tests install it beside GuardSessionKeys; production never does.
func GuardDispositionWrites(fail func(msg string)) {
	dispositionGuard.on.Store(true)
	beads.SetSessionWriteHook(func(kvs map[string]string) {
		if msg := checkDispositionWrite(kvs, dispositionWriter); msg != "" {
			keyViolations.Lock()
			keyViolations.msgs = append(keyViolations.msgs, msg)
			keyViolations.Unlock()
			fail(msg)
		}
	})
}

// checkDispositionWrite is the guard's verdict on one store write's
// metadata: "" when it carries no class key, spends a mint, or writer (the
// stack's reading) finds a fixture or a bypass writer; else the violation.
func checkDispositionWrite(kvs map[string]string, writer func() (at string, bypass, fixture bool)) string {
	class := MetadataPatch{}
	for _, k := range dispositionKeys {
		if v, ok := kvs[k]; ok {
			class[k] = v
		}
	}
	if len(class) == 0 || spendDisposition(class) {
		return ""
	}
	at, bypass, fixture := writer()
	if fixture || bypass {
		return ""
	}
	return fmt.Sprintf("session row disposition keys %v written by hand at %s: write them through a cell (DispositionPatch.Into from Disposition.Changed)\n%s", class, at, stackText())
}

// dispositionWriter reads the stack of a store write: at is its first frames
// past the plumbing, innermost first; fixture when the first is a _test.go
// file's; bypass when a listed bypass writer is on the stack.
func dispositionWriter() (at string, bypass, fixture bool) {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(3, pcs)])
	for {
		f, more := frames.Next()
		if !plumbing(f.File) {
			name := dispositionFuncName(f.Function)
			switch {
			case at == "":
				at = name
				if strings.HasSuffix(f.File, "_test.go") {
					return at, false, true
				}
			case strings.Count(at, " <- ") < 3:
				at += " <- " + name
			}
			if _, ok := dispositionBypass[name]; ok {
				return at, true, false
			}
		}
		if !more {
			return at, false, false
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
