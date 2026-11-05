//go:build integration

package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// localBox is a runtime.ExecProvider whose box is a private tmux server on this
// host: every argv runs locally with "tmux" resolving to a wrapper that pins
// the server to its own -L socket. beforeGuard, when set, runs once before
// the first guarded (if-shell) command.
type localBox struct {
	t           *testing.T
	tmux        string // the wrapper pinning the private socket
	env         []string
	beforeGuard func()
}

func newLocalBox(t *testing.T) *localBox {
	t.Helper()
	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}
	dir := t.TempDir()
	socket := fmt.Sprintf("gc-carrier-%d-%d", os.Getpid(), time.Now().UnixNano())
	wrapper := filepath.Join(dir, "tmux")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+real+" -L "+socket+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A short socket directory: a socket path past sockaddr_un's limit
	// cannot be bound.
	sockDir, err := os.MkdirTemp("/tmp", "gcbox")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	env := []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"), "TMUX_TMPDIR=" + sockDir, "HOME=" + dir}
	b := &localBox{t: t, tmux: wrapper, env: env}
	// Registered after the directory's cleanup, so it runs first.
	t.Cleanup(func() { _, _, _ = b.Exec(context.Background(), "", []string{"tmux", "kill-server"}) })
	return b
}

func (b *localBox) Exec(ctx context.Context, _ string, argv []string) ([]byte, int, error) {
	if b.beforeGuard != nil && slices.Contains(argv, "if-shell") {
		hook := b.beforeGuard
		b.beforeGuard = nil
		hook()
	}
	bin := argv[0]
	if bin == "tmux" {
		// exec.Command resolves the name on this process's PATH, not
		// cmd.Env's: name the wrapper, never the host's tmux.
		bin = b.tmux
	}
	cmd := exec.CommandContext(ctx, bin, argv[1:]...)
	cmd.Env = b.env
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out, exitErr.ExitCode(), nil
	}
	return out, 0, err
}

func (b *localBox) mustTmux(args ...string) string {
	b.t.Helper()
	out, code, err := b.Exec(context.Background(), "", append([]string{"tmux"}, args...))
	if err != nil || code != 0 {
		b.t.Fatalf("tmux %q: code %d, %v", args, code, err)
	}
	return string(out)
}

// paneHas reports whether the box pane shows want, waiting briefly for it.
func (b *localBox) paneHas(want string) bool {
	deadline := time.Now().Add(3 * time.Second)
	for {
		if strings.Contains(b.mustTmux("capture-pane", "-p", "-t", "main"), want) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Kills a carrier guard that real tmux does not hold to: text arrives byte
// for byte under the fence (quotes, semicolons and backslashes included), and an agent respawned as another incarnation
// between the fence read and the send receives no keystroke, with the
// staged buffer deleted.
func TestCarrierFencedNudgeRealTmuxRespawnBetweenCheckAndSend(t *testing.T) {
	box := newLocalBox(t)
	box.mustTmux("new-session", "-d", "-s", "main", "-x", "200", "-y", "20", "cat")
	c := runtime.NewTmuxCarrier(box, "main")
	first := runtime.InputFence{Generation: 1, Epoch: 1, Token: "first"}
	if err := c.PublishInputFence(context.Background(), "s", first); err != nil {
		t.Fatalf("PublishInputFence: %v", err)
	}
	ctx := context.Background()
	if err := c.NudgeFenced(ctx, "s", runtime.TextContent("-x it's; done; a\\b c\\tz"), first); err != nil {
		t.Fatalf("NudgeFenced under the pane's fence: %v", err)
	}
	if !box.paneHas("-x it's; done; a\\b c\\tz") {
		t.Fatalf("pane = %q, want the text byte for byte", box.mustTmux("capture-pane", "-p", "-t", "main"))
	}

	second := runtime.InputFence{Generation: 2, Epoch: 1, Token: "second"}
	box.beforeGuard = func() {
		box.mustTmux(append([]string{"respawn-pane", "-k", "-t", "main", "cat", ";"}, runtime.TmuxPublishInputFenceArgs("main", second)...)...)
	}
	err := c.NudgeFenced(ctx, "s", runtime.TextContent("stale"), first)
	var miss *runtime.FenceMismatchError
	if !errors.As(err, &miss) || miss.Found != second.Digest() || miss.Partial {
		t.Fatalf("NudgeFenced after a respawn = %v, want a whole miss finding the new incarnation", err)
	}
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(box.mustTmux("capture-pane", "-p", "-t", "main"), "stale") {
		t.Fatal("the respawned incarnation received the stale nudge")
	}
	if buffers := strings.TrimSpace(box.mustTmux("list-buffers", "-F", "#{buffer_name}")); buffers != "" {
		t.Fatalf("buffers left after the miss: %q", buffers)
	}
	if err := c.NudgeFenced(ctx, "s", runtime.TextContent("fresh"), second); err != nil || !box.paneHas("fresh") {
		t.Fatalf("NudgeFenced under the new fence = %v, want delivered", err)
	}
}

// Kills a carrier that reads a box tmux it could not reach as an answer:
// real tmux with no server exits 1, which the box shell reports, so a
// guarded command after the server died and a fence read with no server are
// both unread (ErrRuntimeUnavailable), neither a miss nor gone. Only a live
// server without the agent's session answers that the pane is absent.
func TestCarrierFencedInputRealTmuxServerGoneIsUnread(t *testing.T) {
	box := newLocalBox(t)
	box.mustTmux("new-session", "-d", "-s", "main", "-x", "200", "-y", "20", "cat")
	c := runtime.NewTmuxCarrier(box, "main")
	f := runtime.InputFence{Generation: 1, Epoch: 1, Token: "tok"}
	if err := c.PublishInputFence(context.Background(), "s", f); err != nil {
		t.Fatalf("PublishInputFence: %v", err)
	}
	var miss *runtime.FenceMismatchError
	unread := func(op string, err error) {
		t.Helper()
		var u *runtime.CarrierFenceUnreadError
		if !errors.As(err, &u) || u.Partial || !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.As(err, &miss) || runtime.IsSessionGone(err) {
			t.Fatalf("%s = %v, want a whole unread command (ErrRuntimeUnavailable), neither a miss nor gone", op, err)
		}
	}
	box.beforeGuard = func() { box.mustTmux("kill-server") }
	unread("SendKeysFenced after the server died", c.SendKeysFenced(context.Background(), "s", f, "Enter"))
	unread("NudgeFenced with no server", c.NudgeFenced(context.Background(), "s", runtime.TextContent("hi"), f))

	box.mustTmux("new-session", "-d", "-s", "other", "-x", "200", "-y", "20", "cat")
	err := c.NudgeFenced(context.Background(), "s", runtime.TextContent("hi"), f)
	if !errors.As(err, &miss) || miss.Found != runtime.FenceFoundAbsent || miss.Partial {
		t.Fatalf("NudgeFenced with the session gone from a live server = %v, want a whole miss finding the pane absent", err)
	}
}
