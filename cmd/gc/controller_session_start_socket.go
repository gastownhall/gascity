package main

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

const sessionStartCommandPrefix = "session-start:"

// errSessionStartControllerBlocked distinguishes an explicit controller safety
// refusal from an unavailable or older controller, which still permits the
// established generic-poke compatibility fallback.
var errSessionStartControllerBlocked = errors.New("session-start controller blocked admission")

type sessionStartSocketReply string

const (
	sessionStartSocketReplyOK       sessionStartSocketReply = "ok"
	sessionStartSocketReplyFallback sessionStartSocketReply = "fallback"
	sessionStartSocketReplyInvalid  sessionStartSocketReply = "invalid"
	sessionStartSocketReplyBlocked  sessionStartSocketReply = "blocked"
)

type (
	controllerCommandSender    func(cityPath, command string) ([]byte, error)
	sessionStartSocketAdmitter func(sessionID string) sessionStartSocketReply
)

// pokeSessionStartController sends one exact durable session key to the keyed
// start controller. Older, unavailable, or currently legacy-owned controllers
// fall back to the generic reconciler poke so mixed-version operation keeps the
// pre-existing convergence behavior.
func pokeSessionStartController(cityPath, sessionID string) error {
	return pokeSessionStartControllerWith(cityPath, sessionID, sendControllerCommand, pokeController)
}

func pokeSessionStartControllerWith(
	cityPath, sessionID string,
	send controllerCommandSender,
	fallback func(string) error,
) error {
	if err := validateSessionStartAdmission(sessionID, sessionStartAdmissionSocket); err != nil {
		return err
	}
	var exactErr error
	if send != nil {
		response, err := send(cityPath, sessionStartCommandPrefix+sessionID)
		switch {
		case err != nil:
			exactErr = err
		case strings.TrimSpace(string(response)) == string(sessionStartSocketReplyOK):
			return nil
		case strings.TrimSpace(string(response)) == string(sessionStartSocketReplyBlocked):
			return fmt.Errorf("sending exact session-start hint for %q: %w", sessionID, errSessionStartControllerBlocked)
		default:
			exactErr = fmt.Errorf("controller returned %q", strings.TrimSpace(string(response)))
		}
	} else {
		exactErr = errors.New("exact session-start sender is unavailable")
	}
	if fallback == nil {
		return fmt.Errorf("sending exact session-start hint for %q: %w", sessionID, exactErr)
	}
	if err := fallback(cityPath); err != nil {
		// The exact command is only a latency optimization. Preserve the
		// fallback's established error surface when convergence could not be
		// requested through either path.
		return err
	}
	return fmt.Errorf("exact session-start hint for %q in city %q was not admitted (%w); generic fallback requested", sessionID, cityPath, exactErr)
}

func handleSessionStartSocketCmd(conn net.Conn, payload string, admit sessionStartSocketAdmitter) {
	if err := validateSessionStartAdmission(payload, sessionStartAdmissionSocket); err != nil {
		fmt.Fprintf(conn, "%s\n", sessionStartSocketReplyInvalid) //nolint:errcheck // best-effort reply
		return
	}
	if admit == nil {
		fmt.Fprintf(conn, "%s\n", sessionStartSocketReplyFallback) //nolint:errcheck // mixed-version fallback
		return
	}
	fmt.Fprintf(conn, "%s\n", admit(payload)) //nolint:errcheck // best-effort reply
}
