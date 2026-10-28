//go:build !windows

package herdr

import "context"

func (p *Provider) closeSessionPane(ctx context.Context, _ string, paneID string) error {
	return p.c.closePane(ctx, paneID)
}
