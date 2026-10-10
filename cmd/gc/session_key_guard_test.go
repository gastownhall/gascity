package main

import (
	"github.com/gastownhall/gascity/internal/session"
)

// Any write of a session-row key the session field registry does not have
// fails this package's tests (session.GuardSessionKeys).
func init() { session.GuardSessionKeys(func(msg string) { panic(msg) }) }
