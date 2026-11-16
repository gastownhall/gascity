package proxyendpoint

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"time"
)

// Refusals WithSession answers before it opens a socket.
var (
	// ErrEndpointNotLive is returned for an Endpoint whose verdict is not
	// Verdict.Live: dialing anything else can start a stopped store.
	ErrEndpointNotLive = errors.New("proxyendpoint: endpoint is not live; refusing to dial")
	// ErrNoSessionBudget is returned for a session budget that is not positive.
	ErrNoSessionBudget = errors.New("proxyendpoint: session requires a positive budget")
	// ErrNoSessionFunc is returned when WithSession is given no function to run.
	ErrNoSessionFunc = errors.New("proxyendpoint: session requires a function to run")
)

var errSessionNotImplemented = errors.New("proxyendpoint: session helper not implemented")

// WithSession is the RED skeleton: the signature the tests pin, no behavior.
func WithSession(_ context.Context, _ Endpoint, _ string, _ time.Duration, _ func(context.Context, *sql.Conn) error) error {
	return errSessionNotImplemented
}

// withSessionOver is the RED skeleton of the shared single-session body.
func withSessionOver(_ context.Context, _ driver.Connector, _ func(context.Context, *sql.Conn) error) error {
	return errSessionNotImplemented
}
