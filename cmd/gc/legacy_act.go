package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// legacyAct is the legacy controller's destructive verbs. Each is made
// against the facts that decided it (sessionpkg.Decided): under the runtime
// lease, taken without waiting (busy defers to a later tick), it reads the row
// fresh and acts only while the row still carries those facts. A row that
// moved refuses, and a later tick decides again on what it reads.
type legacyAct struct {
	cityPath string
	store    beads.Store
	sp       runtime.Provider
	cfg      *config.City
	stderr   io.Writer
}

// Stop kills d's runtime, resolved by the row's ID, while the row still
// carries d's facts; otherwise it returns sessionpkg.ErrKillPremiseMoved,
// which the caller defers like a busy lease.
func (a legacyAct) Stop(d sessionpkg.Decided) error {
	ctx := sessionpkg.WithKillDecided(context.Background(), d)
	return controllerKillRowCtx(ctx, sessionActor(sessionpkg.ActorController, a.cityPath), a.cityPath, a.store, a.sp, a.cfg, d.Info().ID)
}

// errActDeferred wraps the error of an act that did not run because its
// runtime lease is busy or unreachable: a later tick decides again.
var errActDeferred = errors.New("deferred")

// MarkStopPending writes the drain-ack stop-pending transition at now while
// d's row still carries d's facts. On CommitLanded it returns the decision
// the stop executes against: d's row with the mark (FactsLegacyDrainStop).
// An error is a failed read or write, or errActDeferred.
func (a legacyAct) MarkStopPending(d sessionpkg.Decided, now time.Time) (sessionpkg.CommitResult, sessionpkg.Decided, error) {
	info := d.Info()
	name := strings.TrimSpace(info.SessionNameMetadata)
	if name == "" {
		name = info.ID
	}
	_, release, err := controllerStopLease(a.store, a.cityPath, name, info.ID, a.stderr)
	if err != nil {
		return 0, sessionpkg.Decided{}, fmt.Errorf("%w: %w", errActDeferred, err)
	}
	defer release()
	patch := sessionpkg.DrainAckStopPendingPatch(now)
	res, err := sessionFrontDoor(a.store).Commit(d, patch)
	if err != nil || res != sessionpkg.CommitLanded {
		return res, sessionpkg.Decided{}, err
	}
	return res, sessionpkg.Decide(info.ApplyPatch(patch), sessionpkg.FactsLegacyDrainStop), nil
}
