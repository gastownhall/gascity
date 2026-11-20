package tmux

import "github.com/gastownhall/gascity/internal/runtime"

var _ runtime.IdleWaitFamilyProvider = (*Provider)(nil)

// IdleWaitReadsFamily implements [runtime.IdleWaitFamilyProvider]: the leaf's
// idle wait classifies each capture with the pane's provider family, so it
// reads the turn of every family whose busy reading the leaf trusts
// (busyReadingTrusted): claude's and codex's indicators through the shared
// scan, and grok's Ctrl+c:cancel footer below its live composer
// (grokTurnRunning).
func (p *Provider) IdleWaitReadsFamily(family string) bool {
	return busyReadingTrusted(family)
}
