package main

import (
	"github.com/gastownhall/gascity/internal/api"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// ResetRefusal is api.ResetRefuser over the reconciler's unknown-state
// refusal: why the controller will not act on a reset recorded for a row
// whose state it skips, or "" when it manages the row.
func (cs *controllerState) ResetRefusal(info sessionpkg.Info) string {
	return unknownStateRefusal(info)
}

var _ api.ResetRefuser = (*controllerState)(nil)
