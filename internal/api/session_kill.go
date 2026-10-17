package api

import (
	"context"
	"log"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// killSession is the operator kill of session id for both API kill routes:
// worker.OperatorKill, the protocol `gc session kill` runs, so an API kill
// leaves the same row, records the same session.stopped and worker.operation
// events, and pokes the controller the same way (#7645). force kills past
// live background subagents and a hung runtime lease holder, as --force does.
func (s *Server) killSession(ctx context.Context, store beads.Store, id string, force bool) error {
	handle, err := s.workerHandleForSession(store, id)
	if err != nil {
		return err
	}
	info, infoErr := session.NewStore(beads.SessionStore{Store: store}).Get(id)
	by := s.actor(session.ActorOperator)
	warn := func(msg string) { log.Printf("gc api: session kill: %s", msg) }
	req := worker.OperatorKillRequest{
		SessionID: id,
		Handle:    handle,
		Info:      info,
		InfoErr:   infoErr,
		By:        by,
		Force:     force,
		LiveSubagents: func(ctx context.Context) ([]worker.InFlightSubagent, error) {
			return worker.InFlightBackgroundSubagents(ctx, handle)
		},
		Poke: func() error {
			s.state.Enqueue(reconcilekey.Session(id))
			return nil
		},
		EventActor: "api",
		Warn:       warn,
		Now:        time.Now,
	}
	if !by.City.IsZero() {
		// With no city there is no runtime dir to lease in; the fenced kill
		// then takes what lease it can itself.
		req.Lease = func(ctx context.Context, force bool) (*session.RuntimeLease, bool, error) {
			leaseReq := session.RuntimeLeaseRequest{City: by.City.Path(), Name: info.SessionName, ID: id, TTL: session.RuntimeLeaseTTLFor(s.state.Config())}
			return session.KillRuntimeLease(ctx, session.NewStore(beads.SessionStore{Store: store}), leaseReq, force, warn)
		}
	}
	if ep := s.state.EventProvider(); ep != nil {
		req.Recorder = ep
	}
	if hooks, ok := s.state.(SessionKillHooks); ok {
		req.ResetCircuitBreaker = func(identity string) error { return hooks.ResetSessionCircuitBreaker(id, identity) }
		if infoErr == nil {
			req.RecordStop = func() { hooks.RecordSessionKillStop(info) }
		}
	}
	_, err = worker.OperatorKill(ctx, req)
	return err
}
