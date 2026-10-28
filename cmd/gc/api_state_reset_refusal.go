package main

import (
	"github.com/gastownhall/gascity/internal/api"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// ResetRefusal is api.ResetRefuser: the controller does not act on a
// fresh-restart request for a row in a state it skips, as `gc session reset`
// refuses it.
func (cs *controllerState) ResetRefusal(info sessionpkg.Info) string {
	return unknownStateRefusal(info)
}

var _ api.ResetRefuser = (*controllerState)(nil)
