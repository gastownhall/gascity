//go:build !windows

package herdr

import "context"

func (p *Provider) boundPaneIdentity(_ context.Context, _ string) map[string]string {
	return nil
}

func (p *Provider) clearPaneBindingPlatform(_ string) {}
