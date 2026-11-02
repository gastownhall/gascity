package main

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionacp "github.com/gastownhall/gascity/internal/runtime/acp"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
)

// TestSessionEventPumpActivatesForAutoWithACP pins that a city whose default
// backend has no event stream (tmux, subprocess) but routes some agents to
// ACP gets an active pump: the ACP backend publishes session events through
// the auto router, so ACP deaths poke the reconciler. The stream's opening
// resync earns the trailing-delay poke.
func TestSessionEventPumpActivatesForAutoWithACP(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		pump, pokeCh, cancel := newTestPump(t)
		defer cancel()
		pump.resyncDelay = 300 * time.Millisecond
		wrapped := sessionauto.New(runtime.NewFake(), sessionacp.NewSeamBackedWithDir(dir, sessionacp.Config{}))
		pump.restart(wrapped)
		if !pump.streaming() {
			t.Fatal("streaming() = false for auto(fake, acp), want the ACP event stream active")
		}
		waitPoke(t, pokeCh)
	})
}
