package proxyendpoint

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
)

// OpenSession opens one private MySQL session to bd's proxy on its loopback
// port, with database selected, as `root` with no password: the credentials
// bd's own CLI uses over the same proxy (see DefaultProbeIO). The proxy
// authenticates to its upstream itself, so a caller never dials the upstream.
//
// The session follows the probe's rules for the same reason the probe does:
// bd's idle watcher counts every accepted TCP connection. It costs the proxy
// exactly one dial, it is not registered in internal/doltpool, and its handle
// keeps no idle slot. The caller must call the returned close func, after
// which no connection to the proxy survives. The session's statements run on
// the returned pinned *sql.Conn; a canceled ctx ends the dial and the ping.
func OpenSession(ctx context.Context, port int, database string) (*sql.Conn, func() error, error) {
	if strings.TrimSpace(database) == "" {
		return nil, nil, ErrNoDatabase
	}
	connector, err := probeConnector(port, database, probeDriverTimeout(ProbeSessionTimeout))
	if err != nil {
		return nil, nil, fmt.Errorf("proxy session connector for %s port %d: %w", Host, port, err)
	}
	return openSessionOver(ctx, connector)
}

// openSessionOver dials connector once and hands the dialed session to a
// handle that can never dial a second one (oneSessionConnector), pings it, and
// returns it pinned. On every failure path it closes what it opened.
func openSessionOver(ctx context.Context, connector driver.Connector) (*sql.Conn, func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	session, err := connector.Connect(ctx)
	if err != nil {
		return nil, nil, err
	}
	one := &oneSessionConnector{session: session, driver: connector.Driver()}
	db := sql.OpenDB(one)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(0)
	conn, err := db.Conn(ctx)
	if err != nil {
		one.closeUntaken()
		return nil, nil, withCloseError(err, db.Close())
	}
	closeSession := func() error {
		connErr := conn.Close()
		one.closeUntaken()
		return errors.Join(connErr, db.Close())
	}
	if err := conn.PingContext(ctx); err != nil {
		return nil, nil, withCloseError(err, closeSession())
	}
	return conn, closeSession, nil
}

// withCloseError returns err unchanged when the cleanup that followed it
// succeeded, and both errors otherwise.
func withCloseError(err, closeErr error) error {
	if closeErr == nil {
		return err
	}
	return errors.Join(err, closeErr)
}
