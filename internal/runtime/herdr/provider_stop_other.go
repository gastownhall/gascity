//go:build !windows

package herdr

import "context"

// Stop closes the agent's pane and clears its metadata sidecar. Idempotent.
// The pane resolves through the sidecar binding when the name is gone — the
// earlier "sleep leak" was exactly this gap: name lost ⇒ pane never found ⇒
// closePane never issued ⇒ panes piled up across witness sleep cycles.
func (p *Provider) Stop(name string) error {
	ctx := context.Background()
	pid, err := p.paneID(ctx, name)
	if err == nil && pid != "" {
		_ = p.c.closePane(ctx, pid)
	}
	_ = p.clearMeta(name)
	return nil
}
