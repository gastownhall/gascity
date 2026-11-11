package tap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime/programstatus"
)

// Pane options the tap reads and writes (design §2).
const (
	optNonce  = "@gc_ps_tap"  // written by gc when it arms the pane
	optStatus = "@gc_ps"      // written by the tap
	optText   = "@gc_ps_text" // written by the tap
)

// waitDelay bounds how long a killed tmux client may hold its output pipes.
const waitDelay = 100 * time.Millisecond

// maxErrorOutput bounds the tmux output quoted in a failed write's error.
const maxErrorOutput = 256

// TmuxPublisher publishes writes to one pane through a tmux client addressed
// by socket path. Each write is guarded by the pane's armed nonce, so a tap
// whose pane was re-armed (respawn, heal) can never overwrite the new
// incarnation's record.
type TmuxPublisher struct {
	bin    string
	socket string
	pane   string
	nonce  string
	env    []string
}

// NewTmuxPublisher returns a publisher writing to pane on the tmux server at
// socket with the tmux binary bin. nonce is the pane's "<rand>.<pane>"
// capture nonce. environ is the environment for the tmux client, with TMUX
// and TMUX_PANE removed: the tap inherits the tmux server's global
// environment, which can name another server, so only -S addresses it.
func NewTmuxPublisher(bin, socket, pane, nonce string, environ []string) (*TmuxPublisher, error) {
	if bin == "" {
		return nil, errors.New("tmux publisher: empty tmux binary")
	}
	if socket == "" {
		return nil, errors.New("tmux publisher: empty socket path")
	}
	if !validPaneID(pane) {
		return nil, fmt.Errorf("tmux publisher: invalid pane id %q", pane)
	}
	if !programstatus.ValidNonce(nonce) || !strings.HasSuffix(nonce, "."+pane) {
		return nil, fmt.Errorf("tmux publisher: nonce %q is not bound to pane %s", nonce, pane)
	}
	return &TmuxPublisher{bin: bin, socket: socket, pane: pane, nonce: nonce, env: withoutTmuxEnv(environ)}, nil
}

// Publish runs one guarded write and waits for it, bounded by ctx.
func (p *TmuxPublisher) Publish(ctx context.Context, w Write) error {
	cmd, err := p.command(ctx, w)
	if err != nil {
		return err
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = fmt.Errorf("%w (%w)", ctxErr, err)
		}
		return fmt.Errorf("publishing to pane %s: %w: %s", p.pane, err, clip(out))
	}
	return nil
}

func (p *TmuxPublisher) command(ctx context.Context, w Write) (*exec.Cmd, error) {
	args, err := guardedArgs(p.socket, p.pane, p.nonce, w)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, p.bin, args...)
	cmd.Env = p.env
	cmd.WaitDelay = waitDelay
	return cmd, nil
}

// guardedArgs returns the tmux client arguments for one write:
//
//	-S <socket> if-shell -F -t %N '#{==:#{@gc_ps_tap},<nonce>}'
//	  "set-option -p -t %N @gc_ps '<status>' [; set-option -p -t %N @gc_ps_text '<text>']"
//
// The values come from the codec, whose alphabet has no quote; one that
// does is refused rather than mis-quoted.
func guardedArgs(socket, pane, nonce string, w Write) ([]string, error) {
	if w.Status == "" {
		return nil, errors.New("tmux publisher: empty status value")
	}
	set := func(opt, val string) (string, error) {
		if strings.ContainsAny(val, "'\n") {
			return "", fmt.Errorf("tmux publisher: %s value %q is not quotable", opt, val)
		}
		return "set-option -p -t " + pane + " " + opt + " '" + val + "'", nil
	}
	cmds, err := set(optStatus, w.Status)
	if err != nil {
		return nil, err
	}
	if w.Text != "" {
		text, err := set(optText, w.Text)
		if err != nil {
			return nil, err
		}
		cmds += " ; " + text
	}
	cond := "#{==:#{" + optNonce + "}," + nonce + "}"
	return []string{"-S", socket, "if-shell", "-F", "-t", pane, cond, cmds}, nil
}

// validPaneID reports whether pane is a tmux pane id, "%" and a decimal.
func validPaneID(pane string) bool {
	if len(pane) < 2 || len(pane) > 11 || pane[0] != '%' {
		return false
	}
	for i := 1; i < len(pane); i++ {
		if pane[i] < '0' || pane[i] > '9' {
			return false
		}
	}
	return true
}

// withoutTmuxEnv copies environ without TMUX and TMUX_PANE.
func withoutTmuxEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// clip trims tmux output for an error message.
func clip(out []byte) string {
	out = bytes.TrimSpace(out)
	if len(out) > maxErrorOutput {
		out = append(out[:maxErrorOutput:maxErrorOutput], "..."...)
	}
	return string(out)
}
