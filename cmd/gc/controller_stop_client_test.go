package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestControllerStopResultZeroValueFailsClosed(t *testing.T) {
	var result controllerStopResult
	if result.outcome != controllerStopOutcomeInvalid {
		t.Fatalf("zero-value outcome = %v, want invalid", result.outcome)
	}
	if result.failClosedError() == nil {
		t.Fatal("zero-value result returned no fail-closed error")
	}
}

func TestControllerStopClientAcceptsOnlyExactAcknowledgement(t *testing.T) {
	tests := []struct {
		name        string
		force       bool
		reply       []byte
		readErr     error
		wantOutcome controllerStopOutcome
		wantCommand string
	}{
		{name: "ordinary", reply: []byte("ok\n"), wantOutcome: controllerStopAcknowledged, wantCommand: "stop\n"},
		{name: "force", force: true, reply: []byte("ok\n"), wantOutcome: controllerStopAcknowledged, wantCommand: "stop-force\n"},
		{name: "empty", wantOutcome: controllerStopMayHaveEntered, wantCommand: "stop\n"},
		{name: "partial", reply: []byte("ok"), wantOutcome: controllerStopMayHaveEntered, wantCommand: "stop\n"},
		{name: "malformed", reply: []byte("no\n"), wantOutcome: controllerStopMayHaveEntered, wantCommand: "stop\n"},
		{name: "extra", reply: []byte("ok\nextra"), wantOutcome: controllerStopMayHaveEntered, wantCommand: "stop\n"},
		{name: "oversized", reply: bytes.Repeat([]byte("x"), controllerStopResponseLimit+1), wantOutcome: controllerStopMayHaveEntered, wantCommand: "stop\n"},
		{name: "read error", readErr: errors.New("connection reset"), wantOutcome: controllerStopMayHaveEntered, wantCommand: "stop\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := controllerStopStatInfo(t, "same")
			conn := &scriptedControllerStopConn{reply: tt.reply, readErr: tt.readErr}
			client := controllerStopClient{
				stat:         func(string) (os.FileInfo, error) { return info, nil },
				dial:         func(string, string, time.Duration) (net.Conn, error) { return conn, nil },
				dialTimeout:  time.Second,
				writeTimeout: time.Second,
				readTimeout:  time.Second,
			}

			result := client.stop(t.TempDir(), tt.force)

			if result.outcome != tt.wantOutcome {
				t.Fatalf("outcome = %v, want %v (err=%v)", result.outcome, tt.wantOutcome, result.err)
			}
			if got := conn.writes.String(); got != tt.wantCommand {
				t.Fatalf("command = %q, want %q", got, tt.wantCommand)
			}
			if !conn.closed {
				t.Fatal("connection was not closed")
			}
		})
	}
}

func TestControllerStopClientClassifiesEntryBoundary(t *testing.T) {
	statErr := errors.New("stat failed")
	dialErr := errors.New("dial failed")
	deadlineErr := errors.New("deadline failed")
	writeErr := errors.New("write failed")

	tests := []struct {
		name        string
		stats       []controllerStopStat
		dialErr     error
		conn        *scriptedControllerStopConn
		wantOutcome controllerStopOutcome
		wantDials   int
	}{
		{
			name:        "pre-dial stat failure",
			stats:       []controllerStopStat{{err: statErr}},
			wantOutcome: controllerStopDefinitePreEntryUnavailable,
		},
		{
			name:        "dial failure",
			stats:       []controllerStopStat{{info: controllerStopStatInfo(t, "before-dial")}},
			dialErr:     dialErr,
			wantOutcome: controllerStopDefinitePreEntryUnavailable,
			wantDials:   1,
		},
		{
			name: "post-dial stat failure",
			stats: []controllerStopStat{
				{info: controllerStopStatInfo(t, "before-post-stat")},
				{err: statErr},
			},
			conn:        &scriptedControllerStopConn{},
			wantOutcome: controllerStopMayHaveEntered,
			wantDials:   1,
		},
		{
			name: "socket identity changed",
			stats: []controllerStopStat{
				{info: controllerStopStatInfo(t, "before-replace")},
				{info: controllerStopStatInfo(t, "after-replace")},
			},
			conn:        &scriptedControllerStopConn{},
			wantOutcome: controllerStopMayHaveEntered,
			wantDials:   1,
		},
		{
			name:        "write deadline failure",
			conn:        &scriptedControllerStopConn{writeDeadlineErr: deadlineErr},
			wantOutcome: controllerStopMayHaveEntered,
			wantDials:   1,
		},
		{
			name:        "short write",
			conn:        &scriptedControllerStopConn{writeLimit: 2},
			wantOutcome: controllerStopMayHaveEntered,
			wantDials:   1,
		},
		{
			name:        "write failure",
			conn:        &scriptedControllerStopConn{writeErr: writeErr},
			wantOutcome: controllerStopMayHaveEntered,
			wantDials:   1,
		},
		{
			name:        "read deadline failure",
			conn:        &scriptedControllerStopConn{readDeadlineErr: deadlineErr},
			wantOutcome: controllerStopMayHaveEntered,
			wantDials:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stats := append([]controllerStopStat(nil), tt.stats...)
			if len(stats) == 0 {
				info := controllerStopStatInfo(t, "same-"+tt.name)
				stats = []controllerStopStat{{info: info}, {info: info}}
			}
			statIndex := 0
			dials := 0
			client := controllerStopClient{
				stat: func(string) (os.FileInfo, error) {
					result := stats[statIndex]
					statIndex++
					return result.info, result.err
				},
				dial: func(string, string, time.Duration) (net.Conn, error) {
					dials++
					return tt.conn, tt.dialErr
				},
				dialTimeout:  time.Second,
				writeTimeout: time.Second,
				readTimeout:  time.Second,
			}

			result := client.stop(t.TempDir(), false)

			if result.outcome != tt.wantOutcome {
				t.Fatalf("outcome = %v, want %v (err=%v)", result.outcome, tt.wantOutcome, result.err)
			}
			if dials != tt.wantDials {
				t.Fatalf("dial calls = %d, want %d", dials, tt.wantDials)
			}
		})
	}
}

type controllerStopStat struct {
	info os.FileInfo
	err  error
}

func controllerStopStatInfo(t *testing.T, name string) os.FileInfo {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

type scriptedControllerStopConn struct {
	reply            []byte
	readErr          error
	readDone         bool
	writes           bytes.Buffer
	writeLimit       int
	writeErr         error
	writeDeadlineErr error
	readDeadlineErr  error
	closed           bool
}

func (c *scriptedControllerStopConn) Read(p []byte) (int, error) {
	if c.readDone {
		return 0, io.EOF
	}
	c.readDone = true
	return copy(p, c.reply), c.readErr
}

func (c *scriptedControllerStopConn) Write(p []byte) (int, error) {
	n := len(p)
	if c.writeLimit > 0 && c.writeLimit < n {
		n = c.writeLimit
	}
	_, _ = c.writes.Write(p[:n])
	return n, c.writeErr
}

func (c *scriptedControllerStopConn) Close() error {
	c.closed = true
	return nil
}

func (*scriptedControllerStopConn) LocalAddr() net.Addr  { return scriptedControllerStopAddr("local") }
func (*scriptedControllerStopConn) RemoteAddr() net.Addr { return scriptedControllerStopAddr("remote") }
func (*scriptedControllerStopConn) SetDeadline(time.Time) error {
	return errors.New("unexpected SetDeadline")
}
func (c *scriptedControllerStopConn) SetWriteDeadline(time.Time) error { return c.writeDeadlineErr }
func (c *scriptedControllerStopConn) SetReadDeadline(time.Time) error  { return c.readDeadlineErr }

type scriptedControllerStopAddr string

func (a scriptedControllerStopAddr) Network() string { return "test" }
func (a scriptedControllerStopAddr) String() string  { return string(a) }
