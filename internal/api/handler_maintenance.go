package api

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// maintenanceDisabled is the 503 response returned when
// [maintenance.dolt] enabled=false in city.toml. The detail prefix
// maintenance_disabled lets the CLI surface a targeted error instead of
// collapsing it into the generic cache-not-live fallback bucket.
func maintenanceDisabled() error {
	return apierr.ServiceUnavailable.Msg("maintenance_disabled: [maintenance.dolt] enabled=false in city.toml")
}

// humaHandleMaintenanceStatus is the GET /v0/city/{city}/maintenance/status
// handler. It returns the configured enablement state, the bounded run
// history ring buffer, the in-flight start time (when a cycle is
// executing), and the approximate next-scheduled timestamp so operators
// can answer "is maintenance caught up?" without reading events.
//
// When the maintenance loop is disabled the handler returns 503 so the
// CLI can tell the operator to opt in via city.toml; it intentionally
// does NOT masquerade as 200 with Enabled=false because the endpoint
// exists specifically to surface live state, and an empty-history 200
// would be indistinguishable from a freshly-enabled loop that has not
// yet fired.
func (s *Server) humaHandleMaintenanceStatus(_ context.Context, _ *MaintenanceStatusInput) (*MaintenanceStatusOutput, error) {
	loop := s.state.MaintenanceLoop()
	if loop == nil {
		return nil, maintenanceDisabled()
	}
	cfg := s.state.Config()
	intervalSec := int64(0)
	if cfg != nil {
		intervalSec = int64(cfg.Maintenance.Dolt.IntervalOrDefault().Seconds())
	}

	history := loop.History()
	historyBodies := make([]MaintenanceRunBody, 0, len(history))
	for _, r := range history {
		historyBodies = append(historyBodies, maintenanceRunBodyFromRun(r))
	}

	var lastRun *MaintenanceRunBody
	if n := len(history); n > 0 {
		body := historyBodies[n-1]
		lastRun = &body
	}

	inFlightStart, inFlight := loop.InFlightStart()
	inFlightStr := ""
	if inFlight {
		inFlightStr = inFlightStart.UTC().Format(time.RFC3339)
	}

	nextScheduled := ""
	if lastAt := loop.LastRunAt(); !lastAt.IsZero() && intervalSec > 0 {
		due := lastAt.Add(time.Duration(intervalSec) * time.Second)
		nextScheduled = due.UTC().Format(time.RFC3339)
	}

	out := &MaintenanceStatusOutput{
		CacheAgeHeader: cacheAgeSeconds(s.state.CityBeadStore()),
		Body: MaintenanceStatusBody{
			Enabled:       cfg != nil && cfg.Maintenance.Dolt.Enabled,
			IntervalSec:   intervalSec,
			InFlight:      inFlight,
			InFlightStart: inFlightStr,
			LastRun:       lastRun,
			NextScheduled: nextScheduled,
			History:       historyBodies,
		},
	}
	return out, nil
}

// humaHandleMaintenanceTriggerDoltGC is the POST
// /v0/city/{city}/maintenance/dolt-gc handler. With ?wait=true the handler
// blocks until the run completes and returns 200 with the full Run body;
// otherwise it dispatches a background goroutine and returns 202 with the
// started_at token. Lease contention (scheduler or prior trigger holds
// the mutex) yields 409 with a typed body keyed on
// "maintenance-in-progress" so the CLI can special-case the response.
func (s *Server) humaHandleMaintenanceTriggerDoltGC(ctx context.Context, input *MaintenanceTriggerInput) (*MaintenanceTriggerOutput, error) {
	loop := s.state.MaintenanceLoop()
	if loop == nil {
		return nil, maintenanceDisabled()
	}

	if input.Wait {
		run, err := loop.TriggerNow(ctx)
		if err != nil {
			return nil, maintenanceConflictFromError(err)
		}
		body := maintenanceRunBodyFromRun(run)
		return &MaintenanceTriggerOutput{
			Body: MaintenanceTriggerBody{
				Accepted:  true,
				StartedAt: body.StartedAt,
				Run:       &body,
			},
		}, nil
	}

	// Async path: spin a goroutine that holds the lease and executes the
	// cycle. If the first attempt fails to acquire (some other goroutine
	// got in between), we still report the conflict synchronously so the
	// operator sees the 409 rather than a silent noop. The background
	// goroutine intentionally uses a detached context — the caller's
	// request has already returned — bounded only by the loop's internal
	// timeouts.
	started, err := triggerMaintenanceAsync(loop)
	if err != nil {
		return nil, maintenanceConflictFromError(err)
	}
	return &MaintenanceTriggerOutput{
		Body: MaintenanceTriggerBody{
			Accepted:  true,
			StartedAt: started.UTC().Format(time.RFC3339),
		},
	}, nil
}

// triggerMaintenanceAsync launches a goroutine that runs one maintenance
// cycle. It returns the synthetic start time immediately (time.Now in UTC)
// for the 202 body when the lease is free. When the lease is held, it
// returns an error carrying the in-flight start time.
//
// Factored out so tests can override goroutine spawning without blocking
// on the full cycle.
var triggerMaintenanceAsync = func(loop MaintenanceProvider) (time.Time, error) {
	// Probe: attempt the run synchronously against a canceled context so
	// TriggerNow surfaces the 409 path without executing a cycle. Any
	// other error is bubbled up; on success we would have executed
	// immediately and the 202 semantics collapse to 200, which is not
	// what the bead specifies. To keep async truly async, we instead spin
	// a goroutine and rely on TryLock to report contention.
	//
	// We synchronize on a small channel so the caller sees the correct
	// started_at in the 202 body whether the goroutine ran or bounced
	// off the lease.
	type result struct {
		started time.Time
		err     error
	}
	ch := make(chan result, 1)
	go func() {
		run, err := loop.TriggerNow(context.Background())
		if err != nil {
			ch <- result{err: err}
			return
		}
		ch <- result{started: run.StartedAt}
	}()

	// Give the goroutine up to 100 ms to either:
	//   - acquire the lease and record started (happy path)
	//   - fail fast with MaintenanceInProgressError
	// If it has done neither, it's running the cycle — return the
	// approximate start time so the 202 body still reads correctly.
	select {
	case r := <-ch:
		if r.err != nil {
			return time.Time{}, r.err
		}
		return r.started, nil
	case <-time.After(100 * time.Millisecond):
		if started, ok := loop.InFlightStart(); ok {
			return started, nil
		}
		return time.Now().UTC(), nil
	}
}

// maintenanceConflictFromError converts a supervisor-layer error into the
// appropriate HTTP error. MaintenanceInProgressError becomes a 409
// operation-in-progress — the retryable "another run on this target is active,
// try again once it finishes" conflict, not the terminal wrong-state one — with
// a typed body; anything else surfaces as a 500 with the underlying message.
func maintenanceConflictFromError(err error) error {
	var inProg *supervisor.MaintenanceInProgressError
	if errors.As(err, &inProg) {
		body := MaintenanceInProgressBody{TypeField: "maintenance-in-progress"}
		if !inProg.StartedAt.IsZero() {
			body.StartedAt = inProg.StartedAt.UTC().Format(time.RFC3339)
		}
		enc, encErr := json.Marshal(body)
		if encErr != nil {
			enc = []byte(`{"type":"maintenance-in-progress"}`)
		}
		return apierr.OperationInProgress.Msg("maintenance-in-progress: " + string(enc))
	}
	return apierr.Internal.Msg(err.Error())
}

// maintenanceRunBodyFromRun converts a supervisor.MaintenanceRun into the
// wire body, formatting timestamps as RFC3339 UTC and computing the
// duration. The zero-value run is mapped to a zero-value body so callers
// can call this unconditionally.
func maintenanceRunBodyFromRun(r supervisor.MaintenanceRun) MaintenanceRunBody {
	duration := r.FinishedAt.Sub(r.StartedAt).Seconds()
	if duration < 0 {
		duration = 0
	}
	body := MaintenanceRunBody{
		Stage:           r.Stage,
		Err:             r.Err,
		BeforeBytes:     r.BeforeBytes,
		AfterBytes:      r.AfterBytes,
		SnapshotPath:    r.SnapshotPath,
		DurationSeconds: duration,
	}
	if !r.StartedAt.IsZero() {
		body.StartedAt = r.StartedAt.UTC().Format(time.RFC3339)
	}
	if !r.FinishedAt.IsZero() {
		body.FinishedAt = r.FinishedAt.UTC().Format(time.RFC3339)
	}
	return body
}
