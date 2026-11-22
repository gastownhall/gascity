package main

import (
	"github.com/gastownhall/gascity/internal/api"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// UnknownStateRefusal is api.UnknownStateRefuser: the controller acts on
// neither a wake nor a fresh-restart request for a row in a state it skips,
// as `gc session wake` and `gc session reset` refuse them.
func (cs *controllerState) UnknownStateRefusal(info sessionpkg.Info) string {
	return unknownStateRefusal(info)
}

var _ api.UnknownStateRefuser = (*controllerState)(nil)
