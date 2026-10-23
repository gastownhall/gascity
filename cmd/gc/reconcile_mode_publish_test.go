package main

import (
	"bufio"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

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
	if got, known, err := queryControllerReconcilerMode(t.TempDir(), time.Second); known || err == nil {
		t.Fatalf("queryControllerReconcilerMode with no controller = %v, %v, %v; want unknown with its error", got, known, err)
	}
}

// TestReconcilerModeReplyAnswersOnlyACertainMode (V2-18) proves the reply
// side never defaults to legacy: a wake whose mode this build cannot name,
// or one that carries a v2 planner under another mode, answers nothing, so
// the reader honors the stop request's keys.
func TestReconcilerModeReplyAnswersOnlyACertainMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		wake *controllerWake
		want string
	}{
		{"legacy", &controllerWake{mode: reconcilerLegacy}, `{"session_reconciler":"legacy"}`},
		{"v2", &controllerWake{mode: reconcilerV2, planner: &planner{}}, `{"session_reconciler":"v2"}`},
		{"unnamed mode", &controllerWake{mode: reconcilerV2 + 1}, ""},
		{"v2 planner under legacy", &controllerWake{mode: reconcilerLegacy, planner: &planner{}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := askReconcilerMode(t, tc.wake); got != tc.want {
				t.Fatalf("reply %q, want %q", got, tc.want)
			}
		})
	}
}

// modeQuerySends records the controller exchanges a fake reconcilerModeSend
// was asked for.
type modeQuerySends struct {
	cities, commands []string
	legs             [][3]time.Duration
}

// fakeReconcilerModeSend points reconcilerModeSend at answer, recording each
// exchange. Tests that call it MUST NOT call t.Parallel().
func fakeReconcilerModeSend(t *testing.T, answer func(command string) ([]byte, error)) *modeQuerySends {
	t.Helper()
	sends := &modeQuerySends{}
	prev := reconcilerModeSend
	reconcilerModeSend = func(cityPath, command string, dial, write, read time.Duration) ([]byte, error) {
		sends.cities = append(sends.cities, cityPath)
		sends.commands = append(sends.commands, command)
		sends.legs = append(sends.legs, [3]time.Duration{dial, write, read})
		return answer(command)
	}
	t.Cleanup(func() { reconcilerModeSend = prev })
	return sends
}

// answeredBy is a reconcilerModeSend answer from a controller connection
// handled over wake, as the controller socket handles it: the reply line,
// or the error sendControllerCommandWithTimeouts returns for a connection
// that closed unanswered.
func answeredBy(t *testing.T, wake *controllerWake) func(string) ([]byte, error) {
	return func(command string) ([]byte, error) {
		client, server := net.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			handleControllerConn(server, t.TempDir(), controllerHostingStandalone, func() {}, nil, nil, nil, nil, wake)
		}()
		defer func() { <-done }()
		defer client.Close() //nolint:errcheck
		if _, err := client.Write([]byte(command + "\n")); err != nil {
			return nil, err
		}
		reply, err := bufio.NewReader(client).ReadString('\n')
		if err != nil {
			return nil, controllerCommandError{op: "reading response", err: io.ErrUnexpectedEOF, unresponsive: true}
		}
		return []byte(strings.TrimSpace(reply)), nil
	}
}

// TestQueryControllerReconcilerModeReadsTheControllersAnswer (V2-18) drives
// the client side of the published mode against the controller's own
// handler: a legacy and a v2 controller are read as such, over the verb and
// with the caller's legs; a controller that predates the verb, and replies
// that name no known mode, leave it unknown with the reason.
func TestQueryControllerReconcilerModeReadsTheControllersAnswer(t *testing.T) {
	legacy, err := newControllerWiring(&config.City{}, noEnv, io.Discard)
	if err != nil {
		t.Fatalf("newControllerWiring: %v", err)
	}
	v2 := newTestV2Wiring(t, &config.City{}, io.Discard)
	garbage := func(reply string) func(string) ([]byte, error) {
		return func(string) ([]byte, error) { return []byte(reply), nil }
	}
	for _, tc := range []struct {
		name   string
		answer func(string) ([]byte, error)
		mode   reconcilerMode
		known  bool
	}{
		{"legacy controller", answeredBy(t, legacy.wake), reconcilerLegacy, true},
		{"v2 controller", answeredBy(t, v2.wake), reconcilerV2, true},
		{"controller predating the verb", answeredBy(t, nil), reconcilerLegacy, false},
		{"reply that is not one", garbage("ok"), reconcilerLegacy, false},
		{"reply naming an unknown mode", garbage(`{"session_reconciler":"v3"}`), reconcilerLegacy, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			sends := fakeReconcilerModeSend(t, tc.answer)
			mode, known, err := queryControllerReconcilerMode(city, 300*time.Millisecond)
			if mode != tc.mode || known != tc.known || (err == nil) != tc.known {
				t.Fatalf("query = %v, %v, %v; want %v, %v and an error exactly when unknown", mode, known, err, tc.mode, tc.known)
			}
			leg := 300 * time.Millisecond
			if len(sends.commands) != 1 || sends.commands[0] != reconcilerModeCommand || sends.cities[0] != city || sends.legs[0] != [3]time.Duration{leg, leg, leg} {
				t.Fatalf("sends %+v; want one %q to %s with legs of %s", sends, reconcilerModeCommand, city, leg)
			}
		})
	}
}

// TestQueryControllerReconcilerModeDialsNothingItCannotUse (V2-18): a query
// given no time, and one for a worker site, where no controller socket
// serves the box, are unknown without dialing.
func TestQueryControllerReconcilerModeDialsNothingItCannotUse(t *testing.T) {
	resetControlPlaneForTest(t)
	sends := fakeReconcilerModeSend(t, func(string) ([]byte, error) { return []byte(`{"session_reconciler":"legacy"}`), nil })
	if _, known, err := queryControllerReconcilerMode(t.TempDir(), 0); known || !errors.Is(err, errReconcilerModeNoTime) {
		t.Fatalf("no time: known %v, err %v; want unknown, errReconcilerModeNoTime", known, err)
	}
	box := newWorkerSiteCity(t)
	makeWorkerSite(t, box)
	if _, known, err := queryControllerReconcilerMode(box, time.Second); known || !errors.Is(err, errWorkerSiteNoController) {
		t.Fatalf("worker site: known %v, err %v; want unknown, errWorkerSiteNoController", known, err)
	}
	if len(sends.commands) != 0 {
		t.Fatalf("sends %+v; want none", sends)
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

// laneModeAsks records the cities and legs laneControllerMode was asked
// with.
type laneModeAsks struct {
	cities []string
	legs   []time.Duration
}

// setLaneControllerMode points laneControllerMode at a fixed answer for the
// test, unknown with an error when known is false, and returns the record of
// its asks. Tests that call it MUST NOT call t.Parallel().
func setLaneControllerMode(t *testing.T, mode reconcilerMode, known bool) *laneModeAsks {
	t.Helper()
	asks := &laneModeAsks{}
	prev := laneControllerMode
	laneControllerMode = func(cityPath string, leg time.Duration) (reconcilerMode, bool, error) {
		asks.cities = append(asks.cities, cityPath)
		asks.legs = append(asks.legs, leg)
		if !known {
			return mode, false, errors.New("stub: no controller answered")
		}
		return mode, true, nil
	}
	t.Cleanup(func() { laneControllerMode = prev })
	return asks
}

// TestControllerStopReading (V2-18) pins how a lane pass outside the
// controller reads stop requests: by the controller's latched mode, asked
// with the caller's leg, and, failing safe, live while the mode is unknown,
// with the reason.
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
			asks := setLaneControllerMode(t, tc.mode, tc.known)
			city := t.TempDir()
			got, err := controllerStopReading(city, 42*time.Millisecond)
			if got != tc.want || (err == nil) != tc.known {
				t.Fatalf("controllerStopReading = %d, %v; want %d and an error exactly when unknown", got, err, tc.want)
			}
			if !slices.Equal(asks.cities, []string{city}) || !slices.Equal(asks.legs, []time.Duration{42 * time.Millisecond}) {
				t.Fatalf("asks %+v; want one for %s with a 42ms leg", asks, city)
			}
		})
	}
}

// TestControllerStopReadingWithNoControllerHonorsTheKeys (V2-18) drives
// controllerStopReading's real query at a city no controller serves: the
// mode is unknown, so the request is read live, and the reason is kept.
func TestControllerStopReadingWithNoControllerHonorsTheKeys(t *testing.T) {
	if got, err := controllerStopReading(t.TempDir(), time.Second); got != session.StopRequestsLive || err == nil {
		t.Fatalf("controllerStopReading with no controller = %d, %v; want StopRequestsLive and the reason", got, err)
	}
}

// TestUnknownModeStopNote (V2-18) pins when a pass outside the controller
// notes that the stop request held content only because the mode was
// unknown: a standing request under an unknown mode, and nothing else.
func TestUnknownModeStopNote(t *testing.T) {
	standing := laneRow{stop: session.ReadStopRequest(map[string]string{"generation": "1", session.DrainIntentReasonKey: "idle", session.DrainIntentIncarnationKey: "1"}, session.StopRequestsLive)}
	unknown := errors.New("no controller")
	if note := unknownModeStopNote(standing, unknown); !strings.Contains(note, "unknown (no controller)") {
		t.Fatalf("standing request, unknown mode: note %q", note)
	}
	if note := unknownModeStopNote(standing, nil); note != "" {
		t.Fatalf("known mode: note %q, want none", note)
	}
	if note := unknownModeStopNote(laneRow{}, unknown); note != "" {
		t.Fatalf("no request: note %q, want none", note)
	}
}
