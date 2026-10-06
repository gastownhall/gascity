package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// An acknowledgement counts only for the incarnation that wrote it.
//
// Pool chairs are recycled under the same name and the pane environment is
// per-CHAIR state, so a previous incarnation's `gc runtime drain-ack` leaves
// GC_DRAIN_ACK_SOURCE=agent sitting there for whoever sits down next. Read
// unbound, that residue reads as "the agent already answered" about an agent
// that no longer exists — and since a skip writes nothing, it says so in
// silence, leaving no reminder line and no marker while the chair stays wedged.
//
// These pin the binding in both directions, plus the arm that cannot tell.

// ackBindingUnreadableProvider fails exactly one metadata read, so the tests can
// separate "the binding says stale" from "the binding could not be read".
type ackBindingUnreadableProvider struct {
	*runtime.Fake
	key string
}

func (p *ackBindingUnreadableProvider) GetMeta(name, key string) (string, error) {
	if key == p.key {
		return "", errors.New("provider metadata unreadable")
	}
	return p.Fake.GetMeta(name, key)
}

// THE PIN: a prior incarnation's acknowledgement must not suppress this drain's
// reminder. The fixture row's token is "tok-a"; the ack names somebody else.
func TestDrainReminderFiresThroughPriorIncarnationAckResidue(t *testing.T) {
	e := newDrainReminderEnv(t)
	mustSetMeta(t, e.sp, e.name, reconcilerDrainAckSourceKey, drainAckSourceAgentValue)
	mustSetMeta(t, e.sp, e.name, drainAckRequesterInstanceTokenKey,
		drainAckInstanceTokenDigest("stale-prior-incarnation-token"))

	if got := e.remind(); got != drainReminderDelivered {
		t.Fatalf("outcome = %v, want delivered: residue from a dead incarnation is not an answer", got)
	}
	if got := len(e.nudges()); got != 1 {
		t.Errorf("nudges = %d, want 1", got)
	}
}

// Control: a GENUINE acknowledgement by the CURRENT incarnation still skips, and
// still writes nothing. Without this the pin above would pass for a reminder
// that simply ignores every agent ack.
//
// The row holds the token in the clear and the pane holds only its digest, so
// this is also where the two halves of the encoding have to meet: the reader
// digests the row token before comparing.
func TestDrainReminderStillSkipsCurrentIncarnationAck(t *testing.T) {
	e := newDrainReminderEnv(t)
	mustSetMeta(t, e.sp, e.name, reconcilerDrainAckSourceKey, drainAckSourceAgentValue)
	mustSetMeta(t, e.sp, e.name, drainAckRequesterInstanceTokenKey, drainAckInstanceTokenDigest("tok-a"))
	before := e.beadSnapshot()

	if got := e.remind(); got != drainReminderSkipped {
		t.Fatalf("outcome = %v, want skipped: this incarnation's own acknowledgement stands", got)
	}
	if got := len(e.nudges()); got != 0 {
		t.Errorf("nudges = %d, want 0", got)
	}
	e.assertBeadUnchanged(before)
}

// The two halves of the stamp's encoding meet here rather than by assumption.
// setDrainAck digests the PANE's GC_INSTANCE_TOKEN; the reminder digests the
// ROW's instance_token and compares. Digesting on one side only would make every
// genuine self-ack read as another incarnation's residue — and neither fixture
// above would notice, because each writes the value it expects the other side to
// produce. This one lets the writer produce it.
func TestDrainAckStampRoundTripsFromSetDrainAckToTheReminder(t *testing.T) {
	e := newDrainReminderEnv(t)
	// The pane IS the incarnation the row describes: same session name, same
	// token the fixture bead carries.
	t.Setenv("GC_INSTANCE_TOKEN", "tok-a")
	t.Setenv("GC_TMUX_SESSION", e.name)
	t.Setenv("GC_SESSION_NAME", "")
	ops := &providerDrainOps{sp: e.sp}

	if err := ops.setDrainAck(e.name); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}

	if got := e.remind(); got != drainReminderSkipped {
		t.Fatalf("outcome = %v, want skipped: this incarnation acked for itself", got)
	}
	if got := len(e.nudges()); got != 0 {
		t.Errorf("nudges = %d, want 0", got)
	}
}

// The arm that cannot tell. `gc runtime drain-ack` stamps the requester from the
// pane's own GC_INSTANCE_TOKEN, so an adopted pane whose environment did not
// survive a restart acknowledges with an empty one. That is neither proven
// current nor proven residue. The reminder keeps asking: it is informational,
// and a redundant nudge is noise against a suppressed one costing a chair for
// hours.
func TestDrainReminderRemindsWhenAckCarriesNoIncarnation(t *testing.T) {
	e := newDrainReminderEnv(t)
	mustSetMeta(t, e.sp, e.name, reconcilerDrainAckSourceKey, drainAckSourceAgentValue)

	if got := e.remind(); got != drainReminderDelivered {
		t.Fatalf("outcome = %v, want delivered: an unbindable ack is not proof this incarnation answered", got)
	}
}

// An UNREADABLE binding is different from an absent one: it holds, and unlike
// the old silent skip it leaves a breadcrumb. The silent decline is what hid
// this bug.
func TestDrainReminderHoldsWithBreadcrumbWhenAckBindingUnreadable(t *testing.T) {
	e := newDrainReminderEnv(t)
	mustSetMeta(t, e.sp, e.name, reconcilerDrainAckSourceKey, drainAckSourceAgentValue)
	sp := &ackBindingUnreadableProvider{Fake: e.sp, key: drainAckRequesterInstanceTokenKey}

	if got := e.remindWith(sp); got != drainReminderHeld {
		t.Fatalf("outcome = %v, want held: an unreadable binding is not evidence the ack is stale", got)
	}
	if got := e.meta(drainReminderHoldKey); got != drainReminderHoldAckUnknown {
		t.Fatalf("hold breadcrumb = %q, want %q", got, drainReminderHoldAckUnknown)
	}
}

// The acknowledging agent records which incarnation it was, so the readers above
// have something to bind against. Both identity keys count: a tmux pane names
// itself through GC_TMUX_SESSION, and GC_SESSION_NAME is the fallback for the
// runtimes that do not set it.
//
// This is also the guard against the cross-session pin below regressing into
// "never stamp anything" — a stamp that never lands would satisfy that pin and
// leave every acknowledgement permanently unprovable.
func TestSetDrainAckStampsTheAcknowledgingIncarnation(t *testing.T) {
	for _, identityKey := range []string{"GC_TMUX_SESSION", "GC_SESSION_NAME"} {
		t.Run(identityKey, func(t *testing.T) {
			t.Setenv("GC_INSTANCE_TOKEN", "tok-a")
			t.Setenv("GC_TMUX_SESSION", "")
			t.Setenv("GC_SESSION_NAME", "")
			t.Setenv(identityKey, "gc-city-worker-1")
			sp := runtime.NewFake()
			ops := &providerDrainOps{sp: sp}

			if err := ops.setDrainAck("gc-city-worker-1"); err != nil {
				t.Fatalf("setDrainAck: %v", err)
			}

			got, _ := sp.GetMeta("gc-city-worker-1", drainAckRequesterInstanceTokenKey)
			if want := drainAckInstanceTokenDigest("tok-a"); got != want {
				t.Errorf("%s = %q, want %q", drainAckRequesterInstanceTokenKey, got, want)
			}
			// The security half of the stamp, and the reason the digest exists:
			// SetMeta is an argv channel on the tmux provider, and
			// GC_INSTANCE_TOKEN is a capability this codebase keeps off every
			// command line. The pane must never carry the token itself.
			if got == "tok-a" {
				t.Errorf("%s stamped the raw instance token; the pane must carry only its digest", drainAckRequesterInstanceTokenKey)
			}
			if got, _ := sp.GetMeta("gc-city-worker-1", reconcilerDrainAckSourceKey); got != drainAckSourceAgentValue {
				t.Errorf("ack source = %q, want %q", got, drainAckSourceAgentValue)
			}
		})
	}
}

// `gc runtime drain-ack <name>` takes an explicit target, so an operator or an
// overseer can acknowledge a drain on somebody else's behalf. The caller's
// token is evidence about the CALLER; stamped onto the target's row it reads
// back as agentAckBindingStale — positive proof of residue for an
// acknowledgement that landed seconds ago, which is the one verdict that must
// never be minted by accident. A cross-session ack therefore leaves the stamp
// empty and the acknowledgement reads as unprovable: the reminder keeps asking,
// which is the direction this reader is meant to fail in.
func TestSetDrainAckLeavesStampUnprovableWhenAckingAnotherSession(t *testing.T) {
	t.Setenv("GC_INSTANCE_TOKEN", "tok-operator")
	t.Setenv("GC_TMUX_SESSION", "")
	t.Setenv("GC_SESSION_NAME", "gc-city-mayor")
	sp := runtime.NewFake()
	ops := &providerDrainOps{sp: sp}

	if err := ops.setDrainAck("gc-city-worker-1"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}

	if got, _ := sp.GetMeta("gc-city-worker-1", drainAckRequesterInstanceTokenKey); got != "" {
		t.Errorf("%s = %q, want empty: the acker's own token is not evidence about the session it acked", drainAckRequesterInstanceTokenKey, got)
	}
	if got, _ := sp.GetMeta("gc-city-worker-1", reconcilerDrainAckSourceKey); got != drainAckSourceAgentValue {
		t.Errorf("ack source = %q, want %q: the acknowledgement still lands, only its binding is withheld", got, drainAckSourceAgentValue)
	}
}

// The stamp has the acknowledgement's lifetime, so the erasers must take it.
// A requester stamp left on the pane outlives every drain it described and
// waits to be paired with some later ack's source — and that pairing is exactly
// the "proven stale" evidence class, manufactured out of two unrelated writes.
func TestDrainAckClearPathsRemoveTheIncarnationStamp(t *testing.T) {
	for name, clear := range map[string]func(*providerDrainOps, string) error{
		"clearDrain": (*providerDrainOps).clearDrain,
		"clearReconcilerDrainAckMetadata": func(o *providerDrainOps, session string) error {
			return clearReconcilerDrainAckMetadata(o.sp, session)
		},
	} {
		t.Run(name, func(t *testing.T) {
			sp := runtime.NewFake()
			ops := &providerDrainOps{sp: sp}
			mustSetMeta(t, sp, "worker", reconcilerDrainAckSourceKey, drainAckSourceAgentValue)
			mustSetMeta(t, sp, "worker", drainAckRequesterInstanceTokenKey, drainAckInstanceTokenDigest("tok-a"))

			if err := clear(ops, "worker"); err != nil {
				t.Fatalf("%s: %v", name, err)
			}

			if got, _ := sp.GetMeta("worker", drainAckRequesterInstanceTokenKey); got != "" {
				t.Errorf("%s = %q, want cleared with the acknowledgement it belongs to", drainAckRequesterInstanceTokenKey, got)
			}
		})
	}
}

// The behavioral pin above can only reach the erasers it happens to name, and
// that is how this bug survived its own fix: the tree had a THIRD eraser
// (t3bridge.clearBridgeMeta) which cleared GC_DRAIN_ACK while leaving both
// halves of its provenance behind, so a re-drain of a still-live incarnation
// still read a dead drain's ack as current and skipped in silence. An eraser in
// another package cannot be called from here, and the next one may not exist
// yet — so this census reads the tree instead of enumerating erasers.
//
// The rule it enforces: erasing GC_DRAIN_ACK erases its provenance in the same
// breath. A source with no stamp is merely unprovable and reminds again; a
// source paired with a stale stamp is the wedge.
func TestEveryDrainAckEraserAlsoRemovesItsProvenance(t *testing.T) {
	const (
		ackKey    = "GC_DRAIN_ACK"
		sourceKey = reconcilerDrainAckSourceKey
		stampKey  = drainAckRequesterInstanceTokenKey
	)
	erasers := drainAckEraserCensus(t, ackKey)
	// Non-vacuity: a census that matched nothing would pass in silence, which
	// is the failure mode it exists to replace.
	if len(erasers) < 3 {
		t.Fatalf("census found %d %s erasers (%v), want at least the three known ones — did the scan roots or the key name move?",
			len(erasers), ackKey, slices.Sorted(maps.Keys(erasers)))
	}
	for _, where := range slices.Sorted(maps.Keys(erasers)) {
		removed := erasers[where]
		for _, key := range []string{sourceKey, stampKey} {
			if !removed[key] {
				t.Errorf("%s erases %s but not %s: an acknowledgement's provenance must not outlive the acknowledgement", where, ackKey, key)
			}
		}
	}
}

// drainAckEraserCensus finds every function in the tree that removes ackKey from
// a session, and reports the full set of keys each one removes. "Removes" is any
// call whose name starts with Remove/remove and whose arguments name the key, so
// a new eraser is caught whatever it calls the underlying primitive.
func drainAckEraserCensus(t *testing.T, ackKey string) map[string]map[string]bool {
	t.Helper()
	root := repoRootForLint(t)
	found := map[string]map[string]bool{}
	for _, dir := range []string{"cmd", "internal"} {
		scanRoot := filepath.Join(root, dir)
		err := filepath.WalkDir(scanRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			// testdata holds fixtures that are not required to be valid Go, and
			// nothing there is a live eraser.
			if d.IsDir() {
				if d.Name() == "testdata" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if parseErr != nil {
				return fmt.Errorf("parsing %s: %w", path, parseErr)
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				removed := removedMetaKeys(fn.Body)
				if !removed[ackKey] {
					continue
				}
				rel, relErr := filepath.Rel(root, path)
				if relErr != nil {
					rel = path
				}
				found[rel+":"+fn.Name.Name] = removed
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scanning %s: %v", scanRoot, err)
		}
	}
	return found
}

// removedMetaKeys collects the metadata keys a function body removes. An eraser
// spells its keys one of two ways — inline as removal arguments, or as a slice
// of names the body ranges over — so both shapes are read. Constant identifiers
// resolve against the ones this package defines, because that is how the
// in-package erasers spell them; out-of-package erasers spell them as literals.
func removedMetaKeys(body *ast.BlockStmt) map[string]bool {
	removed := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			if !isMetaRemovalCall(node) {
				return true
			}
			for _, arg := range node.Args {
				if key, ok := drainAckCensusKey(arg); ok {
					removed[key] = true
				}
			}
		case *ast.RangeStmt:
			// `for _, key := range []string{...} { ...Remove...(name, key) }`:
			// the keys are never call arguments, so read the ranged slice — but
			// only when the loop body actually removes something.
			if !containsMetaRemovalCall(node.Body) {
				return true
			}
			for _, elt := range stringSliceElements(node.X) {
				if key, ok := drainAckCensusKey(elt); ok {
					removed[key] = true
				}
			}
		}
		return true
	})
	return removed
}

// isMetaRemovalCall reports whether a call erases a metadata key. Matching on
// the Remove/remove prefix rather than a fixed list of primitives is what lets
// the census see an eraser that reaches the provider through a new wrapper.
func isMetaRemovalCall(call *ast.CallExpr) bool {
	name := ""
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		name = fn.Name
	case *ast.SelectorExpr:
		name = fn.Sel.Name
	}
	return strings.HasPrefix(strings.ToLower(name), "remove")
}

func containsMetaRemovalCall(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && isMetaRemovalCall(call) {
			found = true
		}
		return !found
	})
	return found
}

func stringSliceElements(expr ast.Expr) []ast.Expr {
	composite, ok := expr.(*ast.CompositeLit)
	if !ok {
		return nil
	}
	arrayType, ok := composite.Type.(*ast.ArrayType)
	if !ok {
		return nil
	}
	if ident, ok := arrayType.Elt.(*ast.Ident); !ok || ident.Name != "string" {
		return nil
	}
	return composite.Elts
}

func drainAckCensusKey(expr ast.Expr) (string, bool) {
	switch node := expr.(type) {
	case *ast.BasicLit:
		if node.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(node.Value)
		return value, err == nil
	case *ast.Ident:
		value, ok := map[string]string{
			"reconcilerDrainAckSourceKey":       reconcilerDrainAckSourceKey,
			"drainAckRequesterInstanceTokenKey": drainAckRequesterInstanceTokenKey,
			"reconcilerDrainAckReasonKey":       reconcilerDrainAckReasonKey,
			"reconcilerDrainAckGenerationKey":   reconcilerDrainAckGenerationKey,
		}[node.Name]
		return value, ok
	}
	return "", false
}

// The write side of the degraded-pane arm. A pane whose GC_INSTANCE_TOKEN did
// not survive adoption acknowledges with an empty stamp, and that empty value
// must LAND — overwriting whatever a prior occupant left — rather than leaving
// the old token beside the new source. Left in place, those two unrelated
// writes pair into "proven stale" evidence about an acknowledgement that was
// never bound at all; overwritten, the ack reads as unprovable and the reminder
// keeps asking, which is the direction this reader is meant to fail in.
func TestSetDrainAckOverwritesPriorStampWhenIncarnationUnknown(t *testing.T) {
	t.Setenv("GC_INSTANCE_TOKEN", "")
	// A SELF-ack: the pane knows which session it is, it just lost its token.
	// Without this the empty stamp would come from the cross-session guard and
	// the degraded-pane arm this test names would go unexercised.
	t.Setenv("GC_TMUX_SESSION", "gc-city-worker-1")
	sp := runtime.NewFake()
	ops := &providerDrainOps{sp: sp}
	mustSetMeta(t, sp, "gc-city-worker-1", drainAckRequesterInstanceTokenKey,
		drainAckInstanceTokenDigest("stale-prior-incarnation-token"))

	if err := ops.setDrainAck("gc-city-worker-1"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}

	if got, _ := sp.GetMeta("gc-city-worker-1", drainAckRequesterInstanceTokenKey); got != "" {
		t.Errorf("%s = %q, want empty: a degraded pane must not inherit a dead incarnation's stamp", drainAckRequesterInstanceTokenKey, got)
	}
	if got, _ := sp.GetMeta("gc-city-worker-1", reconcilerDrainAckSourceKey); got != drainAckSourceAgentValue {
		t.Errorf("ack source = %q, want %q", got, drainAckSourceAgentValue)
	}
}
