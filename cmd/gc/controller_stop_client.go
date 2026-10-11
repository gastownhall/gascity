package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

const (
	controllerStopDialTimeout   = 2 * time.Second
	controllerStopWriteTimeout  = 2 * time.Second
	controllerStopReadTimeout   = 10 * time.Second
	controllerStopResponseLimit = 64
)

type controllerStopOutcome uint8

const (
	controllerStopOutcomeInvalid controllerStopOutcome = iota
	controllerStopAcknowledged
	controllerStopDefinitePreEntryUnavailable
	controllerStopMayHaveEntered
)

func (o controllerStopOutcome) String() string {
	switch o {
	case controllerStopOutcomeInvalid:
		return "invalid"
	case controllerStopAcknowledged:
		return "acknowledged"
	case controllerStopDefinitePreEntryUnavailable:
		return "definite_pre_entry_unavailable"
	case controllerStopMayHaveEntered:
		return "may_have_entered"
	default:
		return fmt.Sprintf("controller_stop_outcome(%d)", o)
	}
}

type controllerStopResult struct {
	outcome controllerStopOutcome
	err     error
}

func (r controllerStopResult) failClosedError() error {
	if r.err != nil {
		return r.err
	}
	return fmt.Errorf("controller stop returned non-authoritative outcome %s", r.outcome)
}

type controllerStopClient struct {
	stat         func(string) (os.FileInfo, error)
	dial         func(network, address string, timeout time.Duration) (net.Conn, error)
	dialTimeout  time.Duration
	writeTimeout time.Duration
	readTimeout  time.Duration
}

func sendControllerStop(cityPath string, force bool) controllerStopResult {
	return controllerStopClient{
		stat:         os.Stat,
		dial:         net.DialTimeout,
		dialTimeout:  controllerStopDialTimeout,
		writeTimeout: controllerStopWriteTimeout,
		readTimeout:  controllerStopReadTimeout,
	}.stop(cityPath, force)
}

func (c controllerStopClient) stop(cityPath string, force bool) controllerStopResult {
	socketPath := controllerSocketPath(cityPath)
	before, err := c.stat(socketPath)
	if err != nil {
		return classifiedControllerStopResult(controllerStopDefinitePreEntryUnavailable, "stating socket before dial", err)
	}
	if before == nil {
		return classifiedControllerStopResult(controllerStopDefinitePreEntryUnavailable, "stating socket before dial", errors.New("socket stat returned no identity"))
	}

	conn, err := c.dial("unix", socketPath, c.dialTimeout)
	if err != nil {
		return classifiedControllerStopResult(controllerStopDefinitePreEntryUnavailable, "dialing socket", err)
	}
	defer conn.Close() //nolint:errcheck // the classified exchange result is authoritative

	after, err := c.stat(socketPath)
	if err != nil {
		return classifiedControllerStopResult(controllerStopMayHaveEntered, "stating socket after dial", err)
	}
	if after == nil {
		return classifiedControllerStopResult(controllerStopMayHaveEntered, "stating socket after dial", errors.New("socket stat returned no identity"))
	}
	if !os.SameFile(before, after) {
		return classifiedControllerStopResult(controllerStopMayHaveEntered, "verifying socket identity", errors.New("controller socket changed during dial"))
	}

	if err := conn.SetWriteDeadline(time.Now().Add(c.writeTimeout)); err != nil {
		return classifiedControllerStopResult(controllerStopMayHaveEntered, "setting write deadline", err)
	}
	command := []byte("stop\n")
	if force {
		command = []byte("stop-force\n")
	}
	n, err := conn.Write(command)
	if err != nil {
		return classifiedControllerStopResult(controllerStopMayHaveEntered, "writing command", err)
	}
	if n != len(command) {
		return classifiedControllerStopResult(controllerStopMayHaveEntered, "writing command", fmt.Errorf("short write: wrote %d of %d bytes", n, len(command)))
	}

	if err := conn.SetReadDeadline(time.Now().Add(c.readTimeout)); err != nil {
		return classifiedControllerStopResult(controllerStopMayHaveEntered, "setting read deadline", err)
	}
	reply, err := readControllerStopReply(conn)
	if err != nil {
		return classifiedControllerStopResult(controllerStopMayHaveEntered, "reading acknowledgement", err)
	}
	if !bytes.Equal(reply, []byte("ok\n")) {
		return classifiedControllerStopResult(controllerStopMayHaveEntered, "validating acknowledgement", fmt.Errorf("unexpected response %q", reply))
	}
	return controllerStopResult{outcome: controllerStopAcknowledged}
}

func readControllerStopReply(r io.Reader) ([]byte, error) {
	reply, err := io.ReadAll(io.LimitReader(r, controllerStopResponseLimit+1))
	if err != nil {
		return nil, err
	}
	if len(reply) > controllerStopResponseLimit {
		return nil, fmt.Errorf("response exceeds %d-byte limit", controllerStopResponseLimit)
	}
	return reply, nil
}

func classifiedControllerStopResult(outcome controllerStopOutcome, operation string, err error) controllerStopResult {
	return controllerStopResult{
		outcome: outcome,
		err:     fmt.Errorf("controller stop %s: %s: %w", outcome, operation, err),
	}
}
