package main

import (
	"errors"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
)

// runtimeTokenVerdict classifies a runtime's GC_INSTANCE_TOKEN against the
// token a kill-by-name expects to find. A kill by name proceeds on match,
// absent and gone, as it always has, and is skipped on mismatch and
// unverifiable, so an empty value that came with a read error never passes the
// fence.
type runtimeTokenVerdict int

const (
	// runtimeTokenMatch: the runtime carries the expected token.
	runtimeTokenMatch runtimeTokenVerdict = iota
	// runtimeTokenMismatch: the runtime carries a different, non-empty token,
	// so the name now belongs to another incarnation.
	runtimeTokenMismatch
	// runtimeTokenAbsent: the token is confirmed unset, or the runtime has no
	// metadata store (runtime.ErrMetaUnsupported).
	runtimeTokenAbsent
	// runtimeTokenGone: the session does not exist (runtime.ErrSessionNotFound).
	runtimeTokenGone
	// runtimeTokenUnverifiable: the read failed and could not tell.
	runtimeTokenUnverifiable
)

// classifyRuntimeInstanceToken folds a GC_INSTANCE_TOKEN read into its verdict.
// It classifies errors with errors.Is, never IsSessionGone: that message
// matching reads text such as "not found" in a failed read as gone.
func classifyRuntimeInstanceToken(actual string, err error, expected string) runtimeTokenVerdict {
	actual, err = runtime.MetaValue(actual, err)
	switch {
	case errors.Is(err, runtime.ErrSessionNotFound):
		return runtimeTokenGone
	case err != nil:
		return runtimeTokenUnverifiable
	}
	actual = strings.TrimSpace(actual)
	switch {
	case actual == "":
		return runtimeTokenAbsent
	case actual == strings.TrimSpace(expected):
		return runtimeTokenMatch
	default:
		return runtimeTokenMismatch
	}
}

// readRuntimeInstanceToken reads name's GC_INSTANCE_TOKEN and classifies it
// against expected. The read error is returned with the gone and unverifiable
// verdicts, for the caller's diagnostic.
func readRuntimeInstanceToken(sp runtime.Provider, name, expected string) (runtimeTokenVerdict, error) {
	actual, err := sp.GetMeta(name, "GC_INSTANCE_TOKEN")
	verdict := classifyRuntimeInstanceToken(actual, err, expected)
	if verdict == runtimeTokenGone || verdict == runtimeTokenUnverifiable {
		return verdict, err
	}
	return verdict, nil
}
