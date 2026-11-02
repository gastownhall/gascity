package splittest

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// Semantics selects which production backend's write-time behavior a strict
// leaf models. The two backends disagree — bd/Dolt hard-fails a residence
// violation, SQLite accepts it and corrupts quietly — so a leaf that did not
// declare one would model whichever the kit's author happened to pick and lie
// about the other. The package doc's rule table says which rule follows which
// backend.
//
// The zero value is deliberately invalid: a caller must choose, the same way a
// city's binding chooses a provider.
type Semantics int

const (
	// BdSemantics models the bd/Dolt backend a WORK store runs on: a create
	// whose --id sits outside the database's prefix and a dep add whose
	// endpoint does not resolve are both hard failures, in bd's own wording.
	BdSemantics Semantics = iota + 1

	// SQLiteSemantics models the SQLite backend a relocated COORDINATION CLASS
	// runs on (internal/storebinding/sqlite/beads_engine.go). It accepts a
	// foreign-prefix row and a dangling dep edge exactly as SQLite does — no
	// prefix check on a pinned id, no foreign key on deps — and records each as
	// a ResidenceViolation, so a fixture sees production's outcome instead of
	// an error branch production never takes.
	//
	// The foreign-prefix half is what an UNFENCED SQLite store does. The same
	// OpenEngine that opens the store also fences it to the namespaces its class
	// binding claims, and a fenced leaf refuses a pinned id outside them under
	// either semantics — the fence is the binding's rule, not a backend's. See
	// pinned_id_fence.go; the dep half is unaffected by it.
	SQLiteSemantics
)

// String names the backend this setting models.
func (s Semantics) String() string {
	switch s {
	case BdSemantics:
		return "bd"
	case SQLiteSemantics:
		return "sqlite"
	default:
		return fmt.Sprintf("Semantics(%d)", int(s))
	}
}

// valid reports whether s names a backend the kit knows how to model.
func (s Semantics) valid() bool {
	return s == BdSemantics || s == SQLiteSemantics
}

// ResidenceViolation is one write a SQLiteSemantics store accepted because the
// SQLite backend it models accepts it — a foreign-prefix row inside a class
// database, or a dep edge pointing at a bead that database cannot resolve.
// Production does not error on either; it carries the damage forward, which is
// why the kit records rather than merely rejects.
type ResidenceViolation struct {
	// Op is the store operation that accepted the violation: "create" or
	// "dep-add".
	Op string
	// Detail says what landed and what production does with it afterwards.
	Detail string
}

// String renders the violation for a test failure message.
func (v ResidenceViolation) String() string {
	return v.Op + ": " + v.Detail
}

// residenceLog collects the violations one strict leaf accepted. Strict stores
// are safe for concurrent use because their leaves are, so the log is too.
type residenceLog struct {
	mu         sync.Mutex
	violations []ResidenceViolation
}

// record appends a violation.
func (l *residenceLog) record(op, detail string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.violations = append(l.violations, ResidenceViolation{Op: op, Detail: detail})
}

// take returns the violations recorded so far and clears the log.
func (l *residenceLog) take() []ResidenceViolation {
	l.mu.Lock()
	defer l.mu.Unlock()
	taken := l.violations
	l.violations = nil
	return taken
}

// residenceRecorder is the surface TakeResidenceViolations reaches through the
// storage-capable wrapper variant, which embeds *StrictStore rather than being
// one.
type residenceRecorder interface {
	takeResidenceViolations() []ResidenceViolation
}

// TakeResidenceViolations returns the residence-invariant violations a
// SQLiteSemantics store has accepted so far, and clears them. Claiming them is
// how a fixture says "production's silent acceptance is what I am asserting
// here" — unclaimed violations fail the test at cleanup. A BdSemantics store
// never has any: it rejects at the call site instead.
func TakeResidenceViolations(s beads.Store) []ResidenceViolation {
	recorder, ok := s.(residenceRecorder)
	if !ok {
		return nil
	}
	return recorder.takeResidenceViolations()
}

// failOnUnclaimedResidenceViolations fails t at cleanup for every violation the
// fixture did not claim. It is what keeps a SQLiteSemantics leaf loud while it
// stays faithful to a backend that says nothing.
func failOnUnclaimedResidenceViolations(t *testing.T, s beads.Store) {
	t.Helper()
	t.Cleanup(func() {
		if message := unclaimedViolationsMessage(TakeResidenceViolations(s)); message != "" {
			t.Error(message)
		}
	})
}

// unclaimedViolationsMessage renders the cleanup failure, or "" when the fixture
// left nothing behind.
func unclaimedViolationsMessage(violations []ResidenceViolation) string {
	if len(violations) == 0 {
		return ""
	}
	lines := make([]string, 0, len(violations))
	for _, v := range violations {
		lines = append(lines, "  - "+v.String())
	}
	return fmt.Sprintf("%d residence-invariant violation(s) were accepted by a %s-semantics store:\n%s\n"+
		"SQLite accepts these silently, so production carries the damage forward instead of erroring. "+
		"Fix the store routing, or call splittest.TakeResidenceViolations if asserting the production corruption is the point of this test.",
		len(violations), SQLiteSemantics, strings.Join(lines, "\n"))
}
