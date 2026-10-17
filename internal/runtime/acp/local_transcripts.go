package acp

import "github.com/gastownhall/gascity/internal/runtime"

var (
	_ runtime.LocalTranscriptProvider = (*Provider)(nil)
	_ runtime.LocalTranscriptProvider = (*seamBackedProvider)(nil)
)

// LocalTranscripts implements [runtime.LocalTranscriptProvider]: the agent is
// a child process on this host, so its transcript is here.
func (p *Provider) LocalTranscripts() bool { return true }

// LocalTranscripts passes the underlying provider's declaration through.
func (s *seamBackedProvider) LocalTranscripts() bool { return s.raw.LocalTranscripts() }
