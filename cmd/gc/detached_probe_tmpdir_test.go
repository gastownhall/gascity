package main

import (
	"context"
	"testing"
)

// TestProbeDetachedWork_HostingIsExactUnderAnyTMPDIR pins the live-server
// half of the detached probe contract: a server attested live on its canonical
// socket is classified by what it hosts — alive only when list-sessions names
// exactly this session, dead when it answers without it — and TMPDIR never
// takes part, because tmux resolves its socket from TMUX_TMPDIR alone.
func TestProbeDetachedWork_HostingIsExactUnderAnyTMPDIR(t *testing.T) {
	bindDetachedProbeSocket(t, "gascity")

	tests := []struct {
		name string
		body string
		want detachedProbeStatus
	}{
		{name: "hosting the session is alive", body: `printf 'soak-loop\n'`, want: detachedProbeAlive},
		{name: "hosting another session is dead", body: `printf 'soak-loop-2\n'`, want: detachedProbeDead},
		{name: "an empty session list is dead", body: `printf ''`, want: detachedProbeDead},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TMPDIR", t.TempDir())
			installFakeTmux(t, tt.body)

			got := probeDetachedWork(context.Background(), "tmux:gascity:soak-loop")
			if got.Status != tt.want {
				t.Fatalf("Status = %q, want %q (err=%v)", got.Status, tt.want, got.Err)
			}
		})
	}
}
