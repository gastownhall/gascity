package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// The managed-Dolt recovery governor makes the three entries that can start a
// managed-Dolt recovery behave as one per city: the tick preflight, the
// env-build health ladder and the bd retry-path recover. They share the flight
// registry (managed_dolt_flight.go), so a city has at most one recovery in
// flight; a recovery that fails holds every later env-build and retry-path
// entry off for providerRecoverCooldown, counted from the failure; and a caller
// inside the controller's own process never waits on a recovery past the
// flight window. The governor changes nobody's right to recover.

var (
	// errManagedDoltFlightWindow is what a controller-class caller gets when the
	// flight it joined is still running at managedDoltFlightWindow after the
	// flight started. It is not the caller's context error: the caller carries on
	// and the flight keeps running.
	errManagedDoltFlightWindow = errors.New("managed-dolt recovery still running past the wait window")

	// errManagedDoltRecoveryHeld wraps the error a refused entry returns while a
	// failed flight's hold is in force.
	errManagedDoltRecoveryHeld = errors.New("managed-dolt recovery held")
)

// managedDoltHolds records, per city, the failed flight an entry must not
// repeat until its time is up. Keyed by normalizePathForCompare(cityPath).
var managedDoltHolds sync.Map // normalized cityPath → managedDoltHold

type managedDoltHold struct {
	until time.Time
	err   error // what the failed flight returned
}

// managedDoltControllerCities holds the run context of each city whose
// controller (CityRuntime.run) is this process. Keyed by
// normalizePathForCompare(cityPath).
var managedDoltControllerCities sync.Map // normalized cityPath → *controllerCity

type controllerCity struct{ ctx context.Context }

// managedDoltFlightMarker is set on a flight's context so an entry reached from
// inside the flight runs directly instead of joining the flight it is part of.
type managedDoltFlightMarker struct{ city string }

// registerControllerCity marks cityPath as run by this process's controller
// until the returned function is called. Controller-class callers bound their
// wait on a recovery, and recoveries run under ctx, so shutdown cancels them.
func registerControllerCity(ctx context.Context, cityPath string) func() {
	if cityPath == "" {
		return func() {}
	}
	key := normalizePathForCompare(cityPath)
	registered := &controllerCity{ctx: ctx}
	managedDoltControllerCities.Store(key, registered)
	return func() { managedDoltControllerCities.CompareAndDelete(key, registered) }
}

func controllerCityContext(cityPath string) (context.Context, bool) {
	v, ok := managedDoltControllerCities.Load(normalizePathForCompare(cityPath))
	if !ok {
		return nil, false
	}
	return v.(*controllerCity).ctx, true
}

// startGovernedManagedDoltFlight joins the flight running for cityPath or
// starts one that calls run. A flight for a controller-class city runs under
// the controller's run context; otherwise under ctx. When the flight ends it
// records its outcome (see recordManagedDoltFlightOutcome) before it leaves the
// registry. A panic in run ends the flight with an error instead of the
// process.
func startGovernedManagedDoltFlight(ctx context.Context, cityPath string, run func(context.Context) error) *managedDoltFlight {
	if runCtx, ok := controllerCityContext(cityPath); ok {
		ctx = runCtx
	}
	return joinOrStartManagedDoltFlight(ctx, cityPath, func(flightCtx context.Context) (err error) {
		flightCtx = context.WithValue(flightCtx, managedDoltFlightMarker{normalizePathForCompare(cityPath)}, struct{}{})
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("managed-dolt recovery panicked: %v", r)
			}
			recordManagedDoltFlightOutcome(flightCtx, cityPath, err)
		}()
		return run(flightCtx)
	})
}

// recordManagedDoltFlightOutcome clears the city's hold when its flight
// succeeded and sets one, ending providerRecoverCooldown after now, when it
// failed. A flight whose own context ended was interrupted, not refuted, and
// sets nothing.
func recordManagedDoltFlightOutcome(flightCtx context.Context, cityPath string, err error) {
	key := normalizePathForCompare(cityPath)
	switch {
	case err == nil:
		managedDoltHolds.Delete(key)
	case flightCtx.Err() == nil:
		managedDoltHolds.Store(key, managedDoltHold{until: providerRecoverNow().Add(providerRecoverCooldown()), err: err})
	}
}

// managedDoltHoldError returns the error a refused entry reports while cityPath
// is held, or nil when it is not or the hold has run out.
func managedDoltHoldError(cityPath string) error {
	key := normalizePathForCompare(cityPath)
	v, ok := managedDoltHolds.Load(key)
	if !ok {
		return nil
	}
	hold := v.(managedDoltHold)
	if !providerRecoverNow().Before(hold.until) {
		managedDoltHolds.CompareAndDelete(key, v)
		return nil
	}
	return fmt.Errorf("%w until %s after a failed attempt: %w",
		errManagedDoltRecoveryHeld, hold.until.UTC().Format(time.RFC3339), hold.err)
}

// awaitGovernedManagedDoltFlight waits for flight. A controller-class caller
// waits at most until managedDoltFlightWindow after the flight started and gets
// errManagedDoltFlightWindow when it is still running then; any other caller
// waits for the flight's end. Either way ctx ending returns its error, and the
// flight keeps running.
func awaitGovernedManagedDoltFlight(ctx context.Context, cityPath string, flight *managedDoltFlight) error {
	_, controller := controllerCityContext(cityPath)
	return awaitManagedDoltFlight(ctx, flight, controller)
}

// awaitManagedDoltFlight is awaitGovernedManagedDoltFlight with the window
// choice made by the caller.
func awaitManagedDoltFlight(ctx context.Context, flight *managedDoltFlight, windowed bool) error {
	waitCtx := ctx
	if windowed {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithDeadline(ctx, flight.started.Add(managedDoltFlightWindow))
		defer cancel()
	}
	if finished, err := flight.wait(waitCtx); finished {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errManagedDoltFlightWindow
}

// runGovernedManagedDoltEntry runs run as one entry of the per-city governor: a
// call made from inside a flight, or by the holder of the city's provider slot,
// runs directly; otherwise it is refused while the city is held, else joins or
// starts the city's flight and waits for it as awaitGovernedManagedDoltFlight
// does. A recovery runs under its starter's context, as before, so the starter
// ending it cancels it; a joiner whose ctx ends leaves it running.
func runGovernedManagedDoltEntry(ctx context.Context, cityPath string, run func(context.Context) error) error {
	_, controller := controllerCityContext(cityPath)
	return runWindowedManagedDoltEntry(ctx, cityPath, controller, run)
}

// runWindowedManagedDoltEntry is runGovernedManagedDoltEntry with the wait
// window chosen by the caller.
func runWindowedManagedDoltEntry(ctx context.Context, cityPath string, windowed bool, run func(context.Context) error) error {
	if ctx.Value(managedDoltFlightMarker{normalizePathForCompare(cityPath)}) != nil || holdsProviderSemaphore(ctx, cityPath) {
		return run(ctx)
	}
	if err := managedDoltHoldError(cityPath); err != nil {
		return err
	}
	flight := startGovernedManagedDoltFlight(ctx, cityPath, run)
	return awaitManagedDoltFlight(ctx, flight, windowed)
}

// governedManagedDoltHealth is the env-build entry: the provider health ladder
// without the all-scope readiness barrier, as one governed entry.
func governedManagedDoltHealth(ctx context.Context, cityPath string) error {
	return runGovernedManagedDoltEntry(ctx, cityPath, func(flightCtx context.Context) error {
		return healthBeadsProviderContext(flightCtx, cityPath, false)
	})
}

// governedManagedRecover is the bd retry-path entry: one recover under the
// city's provider slot. It is refused, reporting nil so the caller carries on
// to its retry, while the city is held, while the legacy recover cooldown is
// running, or when a joined flight outlives managedDoltFlightWindow. The wait
// is always windowed: this entry carries no context, so it cannot tell that it
// runs inside the flight it would join. It returns an error only when a recover
// that ran, or that it joined, failed.
func governedManagedRecover(cityPath string) error {
	if _, running := managedDoltFlightStartedAt(cityPath); !running && managedRecoverCoolingDown(cityPath) {
		return nil
	}
	err := runWindowedManagedDoltEntry(context.Background(), cityPath, true, func(flightCtx context.Context) error {
		lastBeadsProviderRecover.Store(normalizePathForCompare(cityPath), providerRecoverNow())
		release, err := acquireProviderSemaphoreForOpContext(flightCtx, cityPath, "recover")
		if err != nil {
			return err
		}
		defer release()
		return recoverManagedBDCommand(cityPath)
	})
	if errors.Is(err, errManagedDoltRecoveryHeld) || errors.Is(err, errManagedDoltFlightWindow) {
		return nil
	}
	return err
}

// managedRecoverCoolingDown reports whether a recover started within
// providerRecoverCooldown, on the same key and clock as the health ladder's.
func managedRecoverCoolingDown(cityPath string) bool {
	v, ok := lastBeadsProviderRecover.Load(normalizePathForCompare(cityPath))
	if !ok {
		return false
	}
	last, ok := v.(time.Time)
	return ok && providerRecoverNow().Sub(last) < providerRecoverCooldown()
}
