package main

import (
	"bufio"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// askReconcilerMode sends reconcilerModeCommand to a controller connection
// handled over wake, as the controller socket handles it, and returns the
// reply line, "" when the connection closed unanswered.
func askReconcilerMode(t *testing.T, wake *controllerWake) string {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleControllerConn(server, t.TempDir(), controllerHostingStandalone, func() {}, nil, nil, nil, nil, wake)
	}()
	if _, err := client.Write([]byte(reconcilerModeCommand + "\n")); err != nil {
		t.Fatalf("writing the verb: %v", err)
	}
	reply, err := bufio.NewReader(client).ReadString('\n')
	_ = client.Close()
	<-done
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("reading the reply: %v", err)
	}
	return strings.TrimSpace(reply)
}

// TestControllerPublishesItsLatchedReconcilerMode (V2-18) proves the
// controller socket answers the mode its wiring latched, from
// newControllerWiring to the reply a reader outside the controller decodes:
// the answer is the latch, not a read of city.toml.
func TestControllerPublishesItsLatchedReconcilerMode(t *testing.T) {
	legacy, err := newControllerWiring(&config.City{}, noEnv, io.Discard)
	if err != nil {
		t.Fatalf("newControllerWiring: %v", err)
	}
	for _, tc := range []struct {
		name   string
		wiring *controllerWiring
		want   reconcilerMode
	}{
		{"legacy", legacy, reconcilerLegacy},
		{"v2", newTestV2Wiring(t, &config.City{}, io.Discard), reconcilerV2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.wiring.wake.mode != tc.want {
				t.Fatalf("wiring's wake carries %v, want %v", tc.wiring.wake.mode, tc.want)
			}
			reply := askReconcilerMode(t, tc.wiring.wake)
			got, known := decodeReconcilerModeReply([]byte(reply))
			if !known || got != tc.want {
				t.Fatalf("reply %q decodes to %v, %v; want %v, true", reply, got, known, tc.want)
			}
		})
	}
}

// TestReconcilerModeUnknown (V2-18) proves a reader outside the controller
// takes no guess when the controller cannot tell it the latched mode: a
// controller with no wake closes the connection unanswered, as a controller
// that predates the verb does, and a city with no controller answers
// nothing.
func TestReconcilerModeUnknown(t *testing.T) {
	if reply := askReconcilerMode(t, nil); reply != "" {
		t.Fatalf("a controller with no wake replied %q, want no reply", reply)
	}
	if got, known := decodeReconcilerModeReply(nil); known {
		t.Fatalf("no reply decodes to %v, true; want unknown", got)
	}
	if got, known := queryControllerReconcilerMode(t.TempDir()); known {
		t.Fatalf("queryControllerReconcilerMode with no controller = %v, true; want unknown", got)
	}
}

// TestDecodeReconcilerModeReply (V2-18) pins the reply's decoding: the two
// latched modes' spellings are known, and anything else is unknown.
func TestDecodeReconcilerModeReply(t *testing.T) {
	for _, tc := range []struct {
		reply string
		mode  reconcilerMode
		known bool
	}{
		{`{"session_reconciler":"legacy"}`, reconcilerLegacy, true},
		{`{"session_reconciler":"v2"}`, reconcilerV2, true},
		{`{"session_reconciler":"v3"}`, reconcilerLegacy, false},
		{`{"session_reconciler":""}`, reconcilerLegacy, false},
		{`{}`, reconcilerLegacy, false},
		{`ok`, reconcilerLegacy, false},
		{``, reconcilerLegacy, false},
	} {
		if mode, known := decodeReconcilerModeReply([]byte(tc.reply)); mode != tc.mode || known != tc.known {
			t.Errorf("decodeReconcilerModeReply(%q) = %v, %v; want %v, %v", tc.reply, mode, known, tc.mode, tc.known)
		}
	}
}

// setLaneControllerMode points laneControllerMode at a fixed answer for the
// test. Tests that call it MUST NOT call t.Parallel().
func setLaneControllerMode(t *testing.T, mode reconcilerMode, known bool) {
	t.Helper()
	prev := laneControllerMode
	laneControllerMode = func(string) (reconcilerMode, bool) { return mode, known }
	t.Cleanup(func() { laneControllerMode = prev })
}

// TestControllerStopReading (V2-18) pins how a lane pass outside the
// controller reads stop requests: by the controller's latched mode, and,
// failing safe, live while the mode is unknown.
func TestControllerStopReading(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mode  reconcilerMode
		known bool
		want  session.StopRequestReading
	}{
		{"latched legacy", reconcilerLegacy, true, session.StopRequestsIgnored},
		{"latched v2", reconcilerV2, true, session.StopRequestsLive},
		{"unknown", reconcilerLegacy, false, session.StopRequestsLive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setLaneControllerMode(t, tc.mode, tc.known)
			if got := controllerStopReading(t.TempDir()); got != tc.want {
				t.Fatalf("controllerStopReading = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestControllerStopReadingWithNoControllerHonorsTheKeys (V2-18) drives
// controllerStopReading's real query at a city no controller serves: the
// mode is unknown, so the request is read live.
func TestControllerStopReadingWithNoControllerHonorsTheKeys(t *testing.T) {
	if got := controllerStopReading(t.TempDir()); got != session.StopRequestsLive {
		t.Fatalf("controllerStopReading with no controller = %d, want StopRequestsLive", got)
	}
}
