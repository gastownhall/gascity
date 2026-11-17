package api

import (
	"context"
	"errors"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/readywork"
)

// ErrReadyWorkConfigNotCurrent is what a ReadyWorkReader or ActiveWorkStepReader
// returns when it cannot show that the config it answers from, and so the set
// of stores it reads, is the one it has published: a config reload or API
// config write is in progress or began during the read, or the last reload
// failed. A reload that is only due is not a reason to refuse. No store
// failed, so the endpoint answers it 503 service-unavailable, not
// store-unavailable.
var ErrReadyWorkConfigNotCurrent = errors.New("the controller's city config is not current: a reload or API config write is in progress, or the last reload failed")

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
//   - The legs come from the config the reader has published, read with its
//     stores as one generation: a rig added, removed or rebound on disk
//     reaches the answer at the next config reload, not before.
//   - It returns ErrReadyWorkConfigNotCurrent while a config reload or API
//     config write is in progress or begins during the read, and after a
//     failed reload until one succeeds, because the stores it holds may then
//     be swapped or no longer match the config it would answer from.
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
// A malformed query is 400, and an unknown query parameter 422. A failed read
// is 503 store-unavailable, with the failing leg named. A controller that does
// not serve ready work, or that is mid-reload or after a failed reload
// (ErrReadyWorkConfigNotCurrent), is 503 service-unavailable. The CLI answers
// either 503 by reading in-process, which reproduces the failure, or succeeds
// where the controller could not.
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
	if errors.Is(err, ErrReadyWorkConfigNotCurrent) {
		return nil, apierr.ServiceUnavailable.Msg(err.Error())
	}
	if err != nil {
		return nil, apierr.StoreUnavailable.Msg(err.Error())
	}
	if rows == nil {
		rows = []readywork.ReadyBead{}
	}
	return &WorkReadyOutput{Body: WorkReadyBody{Items: rows}}, nil
}

// workReadyDescription is the published contract of GET /work/ready.
const workReadyDescription = "Ready work as `gc ready` answers it, read by the controller from the stores it already holds open, over the rigs of the config it has loaded. " +
	"Every store is read live at request time (never from a read cache), so work that became ready before the request is in the answer. " +
	"It reads the city store, the serving (non-suspended) rigs and any relocated class binding, merges them first-store-wins in that order, " +
	"applies the filters, orders and cuts to the limit. Any store that cannot be read fails the whole request with 503 `store-unavailable`; there is no partial answer. " +
	"A rig added, removed or rebound in the city's config takes effect in the answer at the controller's next config reload. " +
	"While a config reload or API config write is in progress, or after a failed reload until one succeeds, the request is refused with 503 `service-unavailable`. " +
	"Either 503 means the caller should read the stores in-process instead."
