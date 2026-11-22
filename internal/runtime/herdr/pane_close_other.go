//go:build !windows

package herdr

import "context"

// closeBoundPane closes paneID, the pane bound to the named session.
func (p *Provider) closeBoundPane(ctx context.Context, _ string, paneID string) error {
	return p.c.closePane(ctx, paneID)
}
