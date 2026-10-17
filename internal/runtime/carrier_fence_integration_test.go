//go:build integration

package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// localBox is an ExecProvider whose box is a private tmux server on this
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
	if b.beforeGuard != nil && len(argv) > 1 && argv[1] == "if-shell" {
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
// for byte under the fence, and an agent respawned as another incarnation
// between the fence read and the send receives no keystroke, with the
// staged buffer deleted.
func TestCarrierFencedNudgeRealTmuxRespawnBetweenCheckAndSend(t *testing.T) {
	box := newLocalBox(t)
	box.mustTmux("new-session", "-d", "-s", "main", "-x", "200", "-y", "20", "cat")
	c := NewTmuxCarrier(box, "main")
	first := InputFence{Generation: 1, Epoch: 1, Token: "first"}
	if err := c.PublishInputFence(context.Background(), "s", first); err != nil {
		t.Fatalf("PublishInputFence: %v", err)
	}
	ctx := context.Background()
	if err := c.NudgeFenced(ctx, "s", TextContent("-x it's; done;"), first); err != nil {
		t.Fatalf("NudgeFenced under the pane's fence: %v", err)
	}
	if !box.paneHas("-x it's; done;") {
		t.Fatalf("pane = %q, want the text byte for byte", box.mustTmux("capture-pane", "-p", "-t", "main"))
	}

	second := InputFence{Generation: 2, Epoch: 1, Token: "second"}
	box.beforeGuard = func() {
		box.mustTmux(append([]string{"respawn-pane", "-k", "-t", "main", "cat", ";"}, TmuxPublishInputFenceArgs("main", second)...)...)
	}
	err := c.NudgeFenced(ctx, "s", TextContent("stale"), first)
	var miss *FenceMismatchError
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
	if err := c.NudgeFenced(ctx, "s", TextContent("fresh"), second); err != nil || !box.paneHas("fresh") {
		t.Fatalf("NudgeFenced under the new fence = %v, want delivered", err)
	}
}

// Kills a carrier that reads a box tmux gone between the fence read and the
// send as delivery: real tmux's if-shell with no server exits 1 with no
// output, which is a miss finding the pane absent. A read with no server at
// all finds it absent too, through the read script's sentinel.
func TestCarrierFencedInputRealTmuxServerGoneIsAbsent(t *testing.T) {
	box := newLocalBox(t)
	box.mustTmux("new-session", "-d", "-s", "main", "-x", "200", "-y", "20", "cat")
	c := NewTmuxCarrier(box, "main")
	f := InputFence{Generation: 1, Epoch: 1, Token: "tok"}
	if err := c.PublishInputFence(context.Background(), "s", f); err != nil {
		t.Fatalf("PublishInputFence: %v", err)
	}
	box.beforeGuard = func() { box.mustTmux("kill-server") }
	err := c.SendKeysFenced(context.Background(), "s", f, "Enter")
	var miss *FenceMismatchError
	if !errors.As(err, &miss) || miss.Found != FenceFoundAbsent || miss.Partial {
		t.Fatalf("SendKeysFenced after the server died = %v, want a whole miss finding the pane absent", err)
	}
	err = c.NudgeFenced(context.Background(), "s", TextContent("hi"), f)
	if !errors.As(err, &miss) || miss.Found != FenceFoundAbsent || miss.Partial {
		t.Fatalf("NudgeFenced with no server = %v, want a whole miss finding the pane absent", err)
	}
}
