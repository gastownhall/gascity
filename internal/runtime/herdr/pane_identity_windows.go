//go:build windows

package herdr

import (
	"context"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

var processIdentity = proctable.ProcessIdentity

func (p *Provider) boundPaneIdentity(ctx context.Context, paneID string) map[string]string {
	if strings.HasPrefix(paneID, "%") {
		return nil
	}
	shellPID, _, err := p.c.processInfo(ctx, paneID)
	if err != nil || shellPID <= 1 {
		return nil
	}
	identity, err := processIdentity(shellPID)
	if err != nil || strings.TrimSpace(identity) == "" {
		return nil
	}
	return map[string]string{
		metaBoundShellPID: strconv.Itoa(shellPID),
		metaBoundShellID:  identity,
	}
}

func (p *Provider) clearPaneBindingPlatform(name string) {
	_ = p.RemoveMeta(name, metaBoundShellPID)
	_ = p.RemoveMeta(name, metaBoundShellID)
}
