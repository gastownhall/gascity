package main

import (
	"context"

	"github.com/gastownhall/gascity/internal/beads"
)

// ExtMsgPublicStore exposes only the Messaging class writer. Public admission
// cannot unwrap the Work store or silently choose another coordination class.
func (cs *controllerState) ExtMsgPublicStore() beads.MailStore { return cs.mailBeadStore() }

// ExtMsgPublicContext owns the relay's lifetime: stopping/replacing the city
// cancels its relay. API test states need not implement this lifecycle seam.
func (cs *controllerState) ExtMsgPublicContext() context.Context { return cs.cacheCtx }
