package proxyendpoint

import (
	"context"
	"database/sql"
	"errors"
	"strings"
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

// WithSession runs fn over exactly one pinned, pinged session to the live proxy
// ep names, logged in to database, and closes everything it opened before it
// returns. It is the transport half of a proxied-store query: what the session
// means is fn's business, so the verdict stays with the caller.
//
// Every refusal is answered before a socket is opened, because a dial is the one
// act that can start a stopped proxied store: ep must already carry a live
// verdict (Inspect's, not a guess), database must be named, budget must be
// positive, fn must exist and ctx must not already be done. The whole session,
// dial included, ends on budget; the driver deadlines sit above it.
//
// The session is never retried. A server's rejection reaches the caller as the
// driver's own *mysql.MySQLError (ERR 1049 for a database that does not exist),
// and fn's error comes back as fn produced it.
func WithSession(ctx context.Context, ep Endpoint, database string, budget time.Duration, fn func(context.Context, *sql.Conn) error) error {
	if !ep.Verdict.Live() {
		return ErrEndpointNotLive
	}
	if strings.TrimSpace(database) == "" {
		return ErrNoDatabase
	}
	if budget <= 0 {
		return ErrNoSessionBudget
	}
	if fn == nil {
		return ErrNoSessionFunc
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	connector, err := probeConnector(ep.Record.Port, database, probeDriverTimeout(budget))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	return withSessionOver(ctx, connector, fn)
}
