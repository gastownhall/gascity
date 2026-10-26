package api

import (
	"context"
	"errors"
	"strings"

	"github.com/gastownhall/gascity/internal/api/apierr"
)

// ErrActiveWorkStepScopeNotHeld is what an ActiveWorkStepReader returns when it
// holds no store for the asked work scope: not the city, and no rig of its
// config at that path. A fresh read opens the store at that path itself, so the
// endpoint answers it 503 service-unavailable and the caller reads in-process.
var ErrActiveWorkStepScopeNotHeld = errors.New("the controller holds no store for this work scope")

// ActiveWorkStepReader is optionally implemented by a State that can answer a
// session's step reminder — its current formula step — over the stores it
// already holds open.
//
// The contract is the hook's in-process read:
//
//   - Every store is read LIVE at request time, never from a read cache, so the
//     answer reflects every write committed before the request, whoever wrote it.
//   - The work store is the one at the asked scope (the city, or the rig at that
//     path), and the graph store is the one the city's class routing gives it.
//   - It answers only under a config it can show is current, and otherwise
//     returns ErrReadyWorkConfigNotCurrent; a scope it holds no store for is
//     ErrActiveWorkStepScopeNotHeld. Either, and any failed read, sends the
//     caller to its in-process read, which is the slower route to the same
//     answer.
//
// A nil step with a nil error is the answer "no current step".
type ActiveWorkStepReader interface {
	ActiveWorkStep(ctx context.Context, assignees []string, scopeRoot string) (*ActiveWorkStep, error)
}

// humaHandleWorkActiveStep is the Huma-typed handler for GET
// /v0/city/{cityName}/work/active-step: a session's step reminder, answered by
// the supervisor from the stores it holds open, through the same resolution the
// hook runs in process.
//
// A blank identity is 400: it would match every unassigned bead. A failed read
// is 503 store-unavailable. A controller that does not serve the step, whose
// config is not current, or that holds no store for the scope is 503
// service-unavailable. The hook answers any of them by reading in process.
func (s *Server) humaHandleWorkActiveStep(ctx context.Context, input *WorkActiveStepInput) (*WorkActiveStepOutput, error) {
	for _, assignee := range input.Assignee {
		if strings.TrimSpace(assignee) == "" {
			return nil, apierr.InvalidRequest.Msg("assignee must not be blank")
		}
	}
	reader, ok := s.state.(ActiveWorkStepReader)
	if !ok {
		return nil, apierr.ServiceUnavailable.Msg("this controller does not serve the active work step")
	}
	step, err := reader.ActiveWorkStep(ctx, input.Assignee, input.ScopeRoot)
	switch {
	case errors.Is(err, ErrReadyWorkConfigNotCurrent), errors.Is(err, ErrActiveWorkStepScopeNotHeld):
		return nil, apierr.ServiceUnavailable.Msg(err.Error())
	case err != nil:
		return nil, apierr.StoreUnavailable.Msg(err.Error())
	}
	return &WorkActiveStepOutput{Body: WorkActiveStepBody{Step: step}}, nil
}

// workActiveStepDescription is the published contract of GET /work/active-step.
const workActiveStepDescription = "A session's current formula step, exactly the bead its step reminder names, read by the controller from the stores it already holds open. " +
	"The work store is the one at `scope_root` (the city when omitted, else the rig at that path) and the graph store is the one the city's class routing gives it; both are read live at request time, never from a read cache. " +
	"The identities are tried in the order given. `step` is absent when there is no current step. " +
	"While a config change is pending or being applied, after a failed reload, or for a scope the controller holds no store for, the request is refused with 503 `service-unavailable`; a failed read is 503 `store-unavailable`. " +
	"Either 503 means the caller should read the stores in-process instead."
