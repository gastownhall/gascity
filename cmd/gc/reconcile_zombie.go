package main

import (
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The zombie-pane classifier (CONTRACT v5 S5, D-20): what a dead row's pane
// says about how its agent died. The start effect's recycle (C5a3) and the
// zombie effect (C5c2) peek the pane and classify it here. It is pure: the
// caller peeks, and passes in the time and the exit facts.

// zombieKind is a peeked pane's class.
type zombieKind uint8

const (
	// zombieEmpty: nothing to record, no write and no event, but the pane is
	// classified. Empty output, or a rate-limit screen the exit decider does
	// not quarantine (legacy then records neither a quarantine nor a crash).
	zombieEmpty       zombieKind = iota
	zombieTerminal               // a terminal provider error: park the row (START-028)
	zombieRateLimited            // a rate-limit screen the exit decider quarantines
	zombieCrashed                // any other output: session.crashed, once per (row, runtime token)
	// zombiePeekError is not a classification: the caller refuses with a
	// row backoff and writes nothing.
	zombiePeekError
)

// zombieClass is a pane's class, with a terminal error's reason.
type zombieClass struct {
	Kind   zombieKind
	Reason string
}

// classifyZombiePane classifies a peek of a dead pane as legacy does: a
// terminal provider error first (the zombie capture, session_reconciler.go's
// markProviderTerminalError branch), then a rate-limit screen under the exit
// decider's rule (checkRateLimitStability), else a crash. exit is the row's
// exit facts (sessionExitFactsInfo); the screen facts are this peek's.
func classifyZombiePane(output string, peekErr error, exit session.ExitFacts) zombieClass {
	switch {
	case peekErr != nil:
		return zombieClass{Kind: zombiePeekError}
	case output == "":
		return zombieClass{Kind: zombieEmpty}
	}
	if reason := runtime.ProviderTerminalErrorReason(output); reason != "" {
		return zombieClass{Kind: zombieTerminal, Reason: reason}
	}
	if !runtime.ContainsProviderRateLimitScreen(output) {
		return zombieClass{Kind: zombieCrashed}
	}
	exit.ScreenAvailable, exit.Screen = true, session.ScreenRateLimit
	if session.DecideSessionExit(exit) == session.ExitRateLimitQuarantine {
		return zombieClass{Kind: zombieRateLimited}
	}
	return zombieClass{Kind: zombieEmpty}
}

// zombieClassPatch is the row write c calls for at now: the terminal or the
// quarantine patch, or nothing.
func zombieClassPatch(c zombieClass, now time.Time) session.MetadataPatch {
	switch c.Kind {
	case zombieTerminal:
		return providerTerminalErrorPatch(c.Reason, now)
	case zombieRateLimited:
		return session.RateLimitQuarantinePatch(now.Add(defaultRateLimitQuarantineDuration))
	}
	return nil
}
