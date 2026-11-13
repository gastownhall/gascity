package runtime_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestSharesHostFSNeedsLocalityAndToken pins the one predicate that reads
// fs.host: it narrows locality=local, so it holds only when the handshake
// declares both. fs.host without locality=local reads as undeclared, and
// locality=local alone (a container on the controller's host) promises no
// shared filesystem view or user.
func TestSharesHostFSNeedsLocalityAndToken(t *testing.T) {
	cases := []struct {
		name string
		info runtime.ProtocolInfo
		want bool
	}{
		{"local and fs.host", runtime.ProtocolInfo{Locality: runtime.ProtocolLocalityLocal, Capabilities: []string{runtime.ProtocolCapabilityFSHost}}, true},
		{"local without fs.host", runtime.ProtocolInfo{Locality: runtime.ProtocolLocalityLocal, Capabilities: []string{runtime.ProtocolCapabilityObserveV1}}, false},
		{"fs.host without locality", runtime.ProtocolInfo{Capabilities: []string{runtime.ProtocolCapabilityFSHost}}, false},
		{"fs.host with a remote locality", runtime.ProtocolInfo{Locality: "remote", Capabilities: []string{runtime.ProtocolCapabilityFSHost}}, false},
		{"zero handshake", runtime.ProtocolInfo{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.info.SharesHostFS(); got != tc.want {
				t.Errorf("SharesHostFS() = %v, want %v", got, tc.want)
			}
		})
	}
	if runtime.ProtocolCapabilityFSHost != "fs.host" {
		t.Errorf("ProtocolCapabilityFSHost = %q, want the wire token %q", runtime.ProtocolCapabilityFSHost, "fs.host")
	}
}

// TestNudgeDelivered pins the one reading of "the nudge reached the
// session" every caller shares: nil, and a delivery the runtime could not
// watch start a turn (runtime.ErrNudgeDeliveredUnobserved), wrapped or not.
// Every other outcome, including a submit that may or may not have taken, is
// not a delivery.
func TestNudgeDelivered(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, true},
		{"delivered unobserved", runtime.ErrNudgeDeliveredUnobserved, true},
		{"wrapped delivered unobserved", fmt.Errorf("pack nudge: %w", runtime.ErrNudgeDeliveredUnobserved), true},
		{"unconfirmed", runtime.ErrNudgeDeliveryUnconfirmed, false},
		{"not sent", runtime.ErrNudgeNotSent, false},
		{"busy", runtime.ErrSessionBusy, false},
		{"gone", fmt.Errorf("session %q: %w", "s", runtime.ErrSessionNotFound), false},
		{"untyped", errors.New("exit status 1"), false},
	}
	for _, tc := range cases {
		if got := runtime.NudgeDelivered(tc.err); got != tc.want {
			t.Errorf("NudgeDelivered(%s) = %t, want %t", tc.name, got, tc.want)
		}
	}
}

// TestInputOutcomeWordsAreOneToOne pins the input-outcome vocabulary as a
// projection of the runtime's sentinels: nine distinct words, each naming a
// distinct sentinel, delivered alone naming none (nil), and exactly the two
// delivered words exiting 0. An unknown word names nothing.
func TestInputOutcomeWordsAreOneToOne(t *testing.T) {
	words := runtime.InputOutcomes()
	if len(words) != 9 {
		t.Fatalf("InputOutcomes() = %v, want the nine words", words)
	}
	seenWord := map[runtime.InputOutcome]bool{}
	seenSentinel := map[error]runtime.InputOutcome{}
	for _, w := range words {
		if seenWord[w] {
			t.Errorf("word %q listed twice", w)
		}
		seenWord[w] = true
		if !w.Known() {
			t.Errorf("listed word %q is not Known", w)
		}
		sentinel := w.Sentinel()
		if w == runtime.InputOutcomeDelivered {
			if sentinel != nil {
				t.Errorf("delivered names %v, want nil", sentinel)
			}
			continue
		}
		if sentinel == nil {
			t.Errorf("word %q names no sentinel", w)
			continue
		}
		if prev, dup := seenSentinel[sentinel]; dup {
			t.Errorf("words %q and %q both name %v", prev, w, sentinel)
		}
		seenSentinel[sentinel] = w
	}
	want := map[runtime.InputOutcome]error{
		runtime.InputOutcomeDelivered:           nil,
		runtime.InputOutcomeDeliveredUnobserved: runtime.ErrNudgeDeliveredUnobserved,
		runtime.InputOutcomeUnconfirmed:         runtime.ErrNudgeDeliveryUnconfirmed,
		runtime.InputOutcomeNotSent:             runtime.ErrNudgeNotSent,
		runtime.InputOutcomeRejected:            runtime.ErrNudgeRejected,
		runtime.InputOutcomeBusy:                runtime.ErrSessionBusy,
		runtime.InputOutcomePendingInteraction:  runtime.ErrNudgeRefusedPendingInteraction,
		runtime.InputOutcomeUnavailable:         runtime.ErrRuntimeUnavailable,
		runtime.InputOutcomeGone:                runtime.ErrSessionNotFound,
	}
	for w, sentinel := range want {
		if got := w.Sentinel(); !errors.Is(got, sentinel) {
			t.Errorf("%q.Sentinel() = %v, want %v", w, got, sentinel)
		}
		wantSuccess := w == runtime.InputOutcomeDelivered || w == runtime.InputOutcomeDeliveredUnobserved
		if w.Success() != wantSuccess {
			t.Errorf("%q.Success() = %t, want %t", w, w.Success(), wantSuccess)
		}
		if runtime.NudgeDelivered(sentinel) != wantSuccess {
			t.Errorf("NudgeDelivered(%q's sentinel) = %t, want %t: the success words are exactly the delivered ones", w, !wantSuccess, wantSuccess)
		}
	}
	if unknown := runtime.InputOutcome("ok"); unknown.Known() || unknown.Sentinel() != nil || unknown.Success() {
		t.Errorf("unknown word %q reads as known, success or a sentinel", unknown)
	}
	if runtime.ExitCodeInputOutcome != 80 {
		t.Errorf("ExitCodeInputOutcome = %d, want 80", runtime.ExitCodeInputOutcome)
	}
}
