package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/reconcilekey"
)

// Keyed controller wake-ups.
//
// Every trigger names the reconcile key it concerns (reconcilekey.Key): the
// city-wide allocator, one session, or the control dispatcher. The legacy
// tick-driven reconciler has exactly two wake signals, so legacyEnqueue is
// the single place keys are folded back onto them: allocator and session
// keys become the generic poke (one full tick), and the control-dispatch key
// becomes the control-dispatcher signal. Every trigger reaches it through
// the controller wake (reconcile_wake.go), which a keyed reconciler points
// at its router; call sites keep passing keys.
//
// Socket protocol. The controller socket keeps its legacy verbs unchanged:
//
//	poke                 key-less; means the allocator
//	control-dispatcher   the control-dispatch key
//
// and additionally accepts
//
//	poke:<json key>      e.g. poke:{"kind":"session","session_id":"gc-1"}
//
// A malformed key degrades to the allocator so a wake is never dropped.
// Older controllers do not know "poke:<json>" and close the connection
// without replying, so senders fall back to the plain "poke" verb, and
// only session keys use the keyed form at all: the allocator and control
// dispatch keys keep their legacy verbs on the wire.

// pokeKeyedCommandPrefix introduces a keyed poke on the controller socket.
const pokeKeyedCommandPrefix = "poke:"

// legacyEnqueue folds keys onto the legacy reconciler's wake channels with
// non-blocking sends, exactly like the key-less pokes it replaces: no keys
// means the allocator, and any number of allocator/session keys is one poke
// (a pending poke already covers them). It reports whether any signal
// landed. Nil channels are skipped.
func legacyEnqueue(pokeCh, controlDispatcherCh chan<- struct{}, keys ...reconcilekey.Key) bool {
	wantPoke := len(keys) == 0
	wantDispatch := false
	for _, k := range keys {
		if k.Normalize().Kind == reconcilekey.KindControlDispatch {
			wantDispatch = true
		} else {
			wantPoke = true
		}
	}
	landed := false
	if wantPoke && pokeCh != nil {
		select {
		case pokeCh <- struct{}{}:
			landed = true
		default: // poke already pending
		}
	}
	if wantDispatch && controlDispatcherCh != nil {
		select {
		case controlDispatcherCh <- struct{}{}:
			landed = true
		default: // control-dispatcher reconcile already pending
		}
	}
	return landed
}

// wireControllerWakeSignals installs the controller wake as cs's enqueue
// path. Both the standalone controller and the supervisor's per-city start
// call it with their controllerWiring's wake, so neither can forget the
// control-dispatcher signal (without it, control-dispatch enqueues from the
// API would be dropped).
func wireControllerWakeSignals(cs *controllerState, wake *controllerWake) {
	cs.wake = wake
}

// parsePokeSocketCommand reports whether line is a poke command and, if so,
// which key it carries. The bare "poke" verb, an empty payload, and a
// malformed payload all mean the allocator.
func parsePokeSocketCommand(line string) (reconcilekey.Key, bool) {
	if line == "poke" {
		return reconcilekey.Allocator(), true
	}
	payload, ok := strings.CutPrefix(line, pokeKeyedCommandPrefix)
	if !ok {
		return reconcilekey.Key{}, false
	}
	key, err := reconcilekey.Decode(payload)
	if err != nil {
		return reconcilekey.Allocator(), true
	}
	return key, true
}

// keyedPokeCommand renders the socket command for key.
func keyedPokeCommand(key reconcilekey.Key) string {
	return pokeKeyedCommandPrefix + key.Encode()
}

// sendKeyedPoke sends key over the controller socket with send. Session
// keys go out as "poke:<json>" first. Only an older controller's answer to
// an unknown verb (it closes the connection without replying) or a reply
// other than "ok" triggers the plain "poke" retry, so a new client never
// loses a wake against an old controller. Any other failure (controller
// unavailable, read timeout) is returned as is: retrying would only repeat
// the dial or the wait. Other keys send "poke".
func sendKeyedPoke(key reconcilekey.Key, send func(command string) ([]byte, error)) error {
	if key = key.Normalize(); key.Kind == reconcilekey.KindSession {
		resp, err := send(keyedPokeCommand(key))
		switch {
		case err == nil && strings.TrimSpace(string(resp)) == "ok":
			return nil
		case err != nil && !keyedPokeUnsupported(err):
			return err
		}
	}
	_, err := send("poke")
	return err
}

// keyedPokeUnsupported reports whether err is an older controller's
// response to the unknown "poke:<json>" verb: the connection closed before
// any reply line.
func keyedPokeUnsupported(err error) bool {
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}

// enqueueController asks the controller for cityPath to reconcile key,
// through the socket. Under the legacy reconciler this is exactly the
// existing wake: the allocator key is pokeController, the control-dispatch
// key is pokeControlDispatch, and a session key is a keyed poke that falls
// back to pokeController (plain poke, then the supervisor) when the keyed
// form is not acknowledged.
func enqueueController(cityPath string, key reconcilekey.Key) error {
	switch key = key.Normalize(); key.Kind {
	case reconcilekey.KindControlDispatch:
		return pokeControlDispatch(cityPath)
	case reconcilekey.KindSession:
		send := func(command string) ([]byte, error) { return sendControllerCommand(cityPath, command) }
		if err := sendKeyedPoke(key, send); err == nil {
			return nil
		}
		// Same fallback as pokeController.
		return pokeSupervisor()
	default:
		return pokeController(cityPath)
	}
}

// pokeControllerForRestart signals the controller to run an immediate
// reconcile for key instead of waiting for the next periodic patrol. It
// does not wait for the controller to act: the restart-requested flag is
// durable, so a signal failure just means the next periodic tick picks up
// the request instead of an immediate one.
//
// This dials the city socket directly rather than going through
// pokeController: pokeController silently falls back to the city-agnostic
// global supervisor socket on any send failure, which would make an
// explicit-restart request for one city spuriously report success via an
// unrelated supervisor.
func pokeControllerForRestart(cityPath string, key reconcilekey.Key) error {
	send := func(command string) ([]byte, error) {
		return sendControllerCommandWithTimeouts(cityPath, command, 2*time.Second, 2*time.Second, 5*time.Second)
	}
	if err := sendKeyedPoke(key, send); err != nil {
		return fmt.Errorf("signaling controller: %w (restart request remains durably set for the next reconcile tick)", err)
	}
	return nil
}
