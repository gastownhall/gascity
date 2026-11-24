//go:build !windows

package herdr

import (
	"context"
	"fmt"
)

// Stop closes the agent's pane and clears its metadata sidecar. Idempotent.
// The pane resolves through the sidecar binding when the name is gone — the
// earlier "sleep leak" was exactly this gap: name lost ⇒ pane never found ⇒
// closePane never issued ⇒ panes piled up across witness sleep cycles.
//
// A session herdr answers it does not have (no agent, no live bound pane, or a
// pane gone before its close) is stopped: nil. So is a session whose pane could
// not be reached because the herdr server is confirmed dead
// (serverConfirmedDead): its panes died with it, and dropping the sidecar
// keeps a restarted server's recycled pane id from resolving to this session.
// Otherwise a herdr that cannot answer (the pane lookup, an exited agent's
// reap or the close fails) is an error, and the sidecar is kept so a retried
// Stop still finds the pane (RUNTIME-CONTRACT-003: runtime.StopForCleanup
// propagates it rather than certify the session gone).
func (p *Provider) Stop(name string) error {
	ctx := context.Background()
	if err := p.closeSessionPane(ctx, name); err != nil && !p.c.serverConfirmedDead(ctx) {
		return fmt.Errorf("herdr: stopping %q: %w", name, err)
	}
	if err := p.clearMeta(name); err != nil {
		return fmt.Errorf("herdr: stopping %q: clearing its metadata: %w", name, err)
	}
	return nil
}
