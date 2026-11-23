package tmux

import "github.com/gastownhall/gascity/internal/runtime"

var _ runtime.LocalTranscriptProvider = (*Provider)(nil)

// LocalTranscripts implements [runtime.LocalTranscriptProvider]: the agent
// runs in a pane of a tmux server on this host, so its transcript is here.
func (p *Provider) LocalTranscripts() bool { return true }
