//go:build windows

package herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

// Attach opens herdr's own client on this console, showing the session's tab,
// and blocks until the user leaves it, as attach blocks elsewhere.
//
// herdr has no direct terminal attach on Windows: `agent attach` answers
// "direct terminal attach is not supported on Windows yet" (measured, herdr
// 0.9.1). The client is the herdr app itself, so the user gets the whole herdr
// session, focused on this one. Leaving the client, by herdr's detach key or by
// closing the console, ends no session (measured).
func (p *Provider) Attach(name string) error {
	cmd, err := p.attachCommand(context.Background(), name)
	if err != nil {
		return err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run() // blocks until the user leaves the client
}

// attachCommand focuses the tab holding name's pane and returns the command
// that opens herdr's client on this session. It focuses the tab rather than
// the agent because a raw session registers no agent, and `agent focus`
// refuses a pane without one (measured).
//
// The lookups need the session's server, so a session whose server is gone
// fails here rather than in the client: herdr's client has no attach-only mode,
// and started for a session with no server it starts a new, empty one
// (measured, for both `herdr --session` and `herdr session attach`).
func (p *Provider) attachCommand(ctx context.Context, name string) (*exec.Cmd, error) {
	pane, err := p.paneID(ctx, name)
	if err != nil {
		return nil, err
	}
	if pane == "" {
		return nil, fmt.Errorf("herdr: attach %q: no pane for this session", name)
	}
	tab, err := p.c.paneTab(ctx, pane)
	if err != nil {
		return nil, err
	}
	if err := p.c.tabFocus(ctx, tab); err != nil {
		return nil, err
	}
	return exec.Command(p.c.bin, "--session", p.c.session), nil
}

// paneTab returns the id of the tab holding paneID.
func (c *client) paneTab(ctx context.Context, paneID string) (string, error) {
	res, err := c.run(ctx, "pane", "get", paneID)
	if err != nil {
		return "", err
	}
	var wrap struct {
		Pane struct {
			TabID string `json:"tab_id"`
		} `json:"pane"`
	}
	if err := json.Unmarshal(res, &wrap); err != nil {
		return "", fmt.Errorf("herdr pane get: decode: %w", err)
	}
	if wrap.Pane.TabID == "" {
		return "", fmt.Errorf("herdr pane get %s: no tab id in the reply", paneID)
	}
	return wrap.Pane.TabID, nil
}

// tabFocus makes tabID the tab herdr's client shows.
func (c *client) tabFocus(ctx context.Context, tabID string) error {
	_, err := c.run(ctx, "tab", "focus", tabID)
	return err
}
