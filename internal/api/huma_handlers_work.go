package api

import (
	"context"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/readywork"
)

// ReadyWorkReader is optionally implemented by a State that can answer the
// city's ready-work question (`gc ready`) over the stores it already holds open.
//
// The contract is `gc ready`'s, not the cache-preferred GET /beads/ready's:
//
//   - Every leg is read LIVE at request time, never from a read cache, so a bead
//     that became ready before the request is in the answer, whoever wrote it.
//   - The legs are the ones `gc ready` reads: the city store, the SERVING rigs,
//     and the relocated class binding, in the plan's order.
//   - It fails loud: any leg that cannot be read fails the whole answer. There
//     is no partial answer, because the CLI it backs has nowhere to say "short".
//
// A State that does not implement it makes the endpoint answer 503, and the CLI
// reads in-process instead.
type ReadyWorkReader interface {
	ReadyWork(ctx context.Context, q readywork.Query) ([]readywork.ReadyBead, error)
}

// humaHandleWorkReady is the Huma-typed handler for GET
// /v0/city/{cityName}/work/ready: `gc ready`, answered by the supervisor from the
// stores it holds open, through the same reader the CLI runs in-process
// (internal/readywork).
//
// A malformed query is 400. Any read failure is 503 with the failing leg named;
// the CLI answers it by reading in-process, which reproduces today's failure,
// or succeeds where the supervisor's own handle could not.
func (s *Server) humaHandleWorkReady(ctx context.Context, input *WorkReadyInput) (*WorkReadyOutput, error) {
	q := input.readyWorkQuery()
	if err := q.Validate(); err != nil {
		return nil, apierr.InvalidRequest.Msg(err.Error())
	}
	reader, ok := s.state.(ReadyWorkReader)
	if !ok {
		return nil, apierr.ServiceUnavailable.Msg("this controller does not serve ready work")
	}
	rows, err := reader.ReadyWork(ctx, q)
	if err != nil {
		return nil, apierr.StoreUnavailable.Msg(err.Error())
	}
	if rows == nil {
		rows = []readywork.ReadyBead{}
	}
	return &WorkReadyOutput{Body: WorkReadyBody{Items: rows}}, nil
}

// workReadyDescription is the published contract of GET /work/ready.
const workReadyDescription = "Ready work exactly as `gc ready` answers it, read by the controller from the stores it already holds open. " +
	"Every store is read live at request time (never from a read cache), so work that became ready before the request is in the answer. " +
	"It reads the city store, the serving (non-suspended) rigs and any relocated class binding, merges them first-store-wins in that order, " +
	"applies the filters, orders and cuts to the limit. Any store that cannot be read fails the whole request with 503; there is no partial answer."
