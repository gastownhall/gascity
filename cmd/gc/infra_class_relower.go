package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// infraRelowerBy is the transition author of the forward pointers the
// re-lowering step closes.
const infraRelowerBy = "gc:storage-migrate"

// convergedInfraBinding is a binding the migration proved and published: it
// is built only from a converged report (convergedBinding), so the
// re-lowering step cannot run before the convergence marker. Before the
// marker every row the binding holds must carry the migration stamp, and a
// row written there would make a re-run refuse the binding as populated.
type convergedInfraBinding struct {
	plan     *storebinding.StoragePlan
	target   infraBindingTarget
	cfg      *config.City
	cityPath string
}

// convergedBinding returns the binding r converged on, and false for every
// other outcome. A genesis has none: it copied no row, so no deliver-now unit
// moved.
func (r infraMigrationReport) convergedBinding(plan *storebinding.StoragePlan, cityPath string, cfg *config.City) (convergedInfraBinding, bool) {
	if !r.serving() || r.Outcome != infraMigrationConverged || plan == nil {
		return convergedInfraBinding{}, false
	}
	return convergedInfraBinding{plan: plan, target: r.Target, cfg: cfg, cityPath: cityPath}, true
}

// openEngine opens the converged binding's engine as the runtime serves it
// (openPlannedBindingEngine): under its mint prefix and namespace fence, never
// through the migration's unfenced writer handle.
func (b convergedInfraBinding) openEngine(rec events.Recorder) (beads.Store, io.Closer, error) {
	store, closer, _, err := openPlannedBindingEngine(b.plan, b.target.Binding, b.cfg, b.cityPath, rec)
	return store, closer, err
}

// newInfraRelowerStore is the op store the step writes through: a lane store
// over engine, minting under the prefix the runtime's lane host discovers.
// Overridden by tests, which fault its batches or inspect its engine.
var newInfraRelowerStore = func(engine beads.Store) (*opjournal.Store, error) {
	return opjournal.NewLaneStore(beads.SessionStore{Store: engine}, cliOpClock)
}

// relowerConvergedNowUnits is the migration's last step: it re-lowers, under
// the binding's prefix, every open deliver-now unit the copy carried in with
// its derived interrupt pinned under the work store's prefix
// (opjournal.Store.RelowerNowUnits), and reports on stderr what it moved and
// what it left for the lanes, which run every unit by identity. The error is
// a step that did not finish: a store error, or a unit a writer changed
// under it.
func relowerConvergedNowUnits(ctx context.Context, b convergedInfraBinding, rec events.Recorder, logPrefix string, stderr io.Writer) (opjournal.RelowerReport, error) {
	engine, closer, err := b.openEngine(rec)
	if err != nil {
		return opjournal.RelowerReport{}, err
	}
	rep, runErr := relowerNowUnitsOn(ctx, b, engine)
	closeErr := closer.Close()
	printRelowerReport(rep, logPrefix, stderr)
	if closeErr != nil {
		closeErr = fmt.Errorf("closing binding %q: %w", b.target.Binding, closeErr)
	}
	return rep, errors.Join(runErr, closeErr)
}

// relowerNowUnitsOn runs the step on the binding's engine. A binding with no
// lowered deliver-now message has no unit to move or report: the step reads
// that with keyed reads, opens no lane store and writes nothing, so a
// binding that cannot apply guarded batches (and so hosts no lane) migrates
// as it did before the step existed.
func relowerNowUnitsOn(ctx context.Context, b convergedInfraBinding, engine beads.Store) (opjournal.RelowerReport, error) {
	has, err := opjournal.HasLoweredNowMessages(beads.SessionStore{Store: engine})
	if err != nil {
		return opjournal.RelowerReport{}, fmt.Errorf("reading binding %q: %w", b.target.Binding, err)
	}
	if !has {
		return opjournal.RelowerReport{}, nil
	}
	store, err := newInfraRelowerStore(engine)
	if err != nil {
		return opjournal.RelowerReport{}, fmt.Errorf("opening the op store of binding %q: %w", b.target.Binding, err)
	}
	return store.RelowerNowUnits(ctx, infraRelowerBy)
}

// printRelowerReport writes one stderr line per unit moved and per unit left,
// and one for the rows this build cannot read.
func printRelowerReport(rep opjournal.RelowerReport, logPrefix string, stderr io.Writer) {
	for _, r := range rep.Relowered {
		fmt.Fprintf(stderr, "%s: re-lowered deliver-now message %s under %s: its interrupt %s is now %s\n", //nolint:errcheck // best-effort stderr
			logPrefix, r.Message, rep.Prefix, r.From, r.To)
	}
	for _, l := range rep.Left {
		fmt.Fprintf(stderr, "%s: left deliver-now message %s where it is: %s; its lane runs it by identity\n", //nolint:errcheck // best-effort stderr
			logPrefix, l.Message, relowerLeftWhy(l))
	}
	if n := len(rep.Quarantined); n > 0 {
		ids := make([]string, 0, n)
		for _, q := range rep.Quarantined {
			ids = append(ids, q.ID)
		}
		fmt.Fprintf(stderr, "%s: %d open op rows this build cannot read were left as they are: %s\n", //nolint:errcheck // best-effort stderr
			logPrefix, n, infraStrandedIDList(ids))
	}
}

// relowerLeftWhy says why the step left unit l.
func relowerLeftWhy(l opjournal.RelowerLeft) string {
	switch l.Reason {
	case opjournal.RelowerInFlight:
		return "its unit was taken before the stop or may have run, so a copy could run it twice"
	case opjournal.RelowerAmbiguous:
		return fmt.Sprintf("it has %d open derived interrupts (%s)", len(l.Interrupts), infraStrandedIDList(l.Interrupts))
	case opjournal.RelowerHomeTaken:
		return fmt.Sprintf("%s is already held by another row", l.Home)
	default:
		return string(l.Reason)
	}
}

// relowerSuccessClause is the clause the success line carries for the units
// the step moved, "" when it moved none, so a city without lane ops prints
// what it always printed.
func relowerSuccessClause(rep opjournal.RelowerReport) string {
	n := len(rep.Relowered)
	if n == 0 {
		return ""
	}
	unit := "units"
	if n == 1 {
		unit = "unit"
	}
	return fmt.Sprintf("; re-lowered %d deliver-now %s under %s", n, unit, rep.Prefix)
}

// relowerFailureAdvice is the operator message of a re-lowering step that did
// not finish on a converged binding.
func relowerFailureAdvice(logPrefix, binding string, err error) string {
	return fmt.Sprintf("%s: the infrastructure classes converged on %s, but re-lowering deliver-now units did not finish: %s. Stop every writer and run `%s` again to finish; the city can start meanwhile, and its lanes run each unit by identity.",
		logPrefix, binding, strings.TrimSpace(err.Error()), storageClearInstruction())
}
