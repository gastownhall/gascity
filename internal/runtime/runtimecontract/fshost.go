package runtimecontract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// This file is the fshost group: what a script declaring fs.host promises,
// that its boxes share the controller's host filesystem and user
// (runtime.ProtocolInfo.SharesHostFS). The promise is checked, not trusted:
// a box's command copies a file the runner wrote and records its uid, and
// the runner reads both back on its own host. Every probe SKIPs unless the
// handshake declares fs.host.

// fsHostSettle bounds how long the runner waits for a box's command to
// write what it was asked to: the command runs once the box is up, which
// may be after start returns.
const fsHostSettle = 10 * time.Second

// fsHostPoll is how often the runner looks for those files.
const fsHostPoll = 50 * time.Millisecond

// fsHostConfig is a start config the fshost group sends. It is encoded
// without HTML escaping, so the shell commands it carries reach the script
// as written.
type fsHostConfig struct {
	WorkDir    string   `json:"work_dir,omitempty"`
	Command    string   `json:"command,omitempty"`
	OverlayDir string   `json:"overlay_dir,omitempty"`
	PreStart   []string `json:"pre_start,omitempty"`
}

// startFSHost starts name with cfg.
func (r *runner) startFSHost(ctx context.Context, name string, cfg fsHostConfig) outcome {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(cfg) // a struct of strings always encodes
	return r.opTimeout(ctx, r.opts.StartTimeout, bytes.TrimRight(buf.Bytes(), "\n"), "start", name)
}

// fsHostDeclared is the fshost group's gate.
func fsHostDeclared(hs runtime.ProtocolInfo) (Status, string, bool) {
	if !hs.Has(runtime.ProtocolCapabilityFSHost) {
		return StatusSkip, "fs.host not declared", false
	}
	return StatusPass, "", true
}

// fsHostDir makes a fresh directory under the run's work dir for one probe.
func (r *runner) fsHostDir() (string, error) {
	return os.MkdirTemp(r.opts.WorkDir, "gc-rpp-fshost-")
}

// awaitFile returns path's content once it is non-empty, within
// fsHostSettle.
func awaitFile(ctx context.Context, path string) (string, error) {
	deadline := time.Now().Add(fsHostSettle)
	for {
		data, err := os.ReadFile(path)
		if err == nil && len(bytes.TrimSpace(data)) > 0 {
			return string(data), nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%s did not appear on this host within %s", path, fsHostSettle)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(fsHostPoll):
		}
	}
}

// startFSHostOrFail starts name with cfg and returns a failing status if it
// cannot.
func startFSHostOrFail(ctx context.Context, r *runner, name string, cfg fsHostConfig) (Status, string) {
	res := r.startFSHost(ctx, name, cfg)
	switch {
	case res.unsupported:
		return StatusFail, "start returned exit 2; start is a required op"
	case res.err != nil:
		return StatusFail, "start failed: " + res.err.Error()
	}
	return StatusPass, ""
}

func probeFSHostSharedView(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if status, detail, ok := fsHostDeclared(hs); !ok {
		return status, detail
	}
	dir, err := r.fsHostDir()
	if err != nil {
		return StatusFail, "making the probe's work dir: " + err.Error()
	}
	defer func() { _ = os.RemoveAll(dir) }()
	in, out := filepath.Join(dir, "gc-rpp-fshost-in"), filepath.Join(dir, "gc-rpp-fshost-out")
	want := fmt.Sprintf("gc-rpp-fshost-%d", time.Now().UnixNano())
	if err := os.WriteFile(in, []byte(want), 0o644); err != nil {
		return StatusFail, "writing the probe file: " + err.Error()
	}
	name := r.nextName()
	cmd := "cp " + shellquote.Quote(in) + " " + shellquote.Quote(out) + "; exec " + r.opts.Command
	if status, detail := startFSHostOrFail(ctx, r, name, fsHostConfig{WorkDir: dir, Command: cmd}); status != StatusPass {
		return status, detail
	}
	defer r.stop(ctx, name)
	got, err := awaitFile(ctx, out)
	switch {
	case err != nil:
		return StatusFail, "the box's copy of the probe file never reached this host: " + err.Error()
	case got != want:
		return StatusFail, fmt.Sprintf("the box's copy reads %q, want %q (the box sees another file at the same path)", got, want)
	}
	return StatusPass, "the box read the caller's file and wrote its copy where the caller reads it"
}

func probeFSHostSameUser(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if status, detail, ok := fsHostDeclared(hs); !ok {
		return status, detail
	}
	dir, err := r.fsHostDir()
	if err != nil {
		return StatusFail, "making the probe's work dir: " + err.Error()
	}
	defer func() { _ = os.RemoveAll(dir) }()
	out := filepath.Join(dir, "gc-rpp-fshost-uid")
	name := r.nextName()
	cmd := "id -u | tee " + shellquote.Quote(out) + "; exec " + r.opts.Command
	if status, detail := startFSHostOrFail(ctx, r, name, fsHostConfig{WorkDir: dir, Command: cmd}); status != StatusPass {
		return status, detail
	}
	defer r.stop(ctx, name)
	got, err := awaitFile(ctx, out)
	if err != nil {
		return StatusFail, "the box's uid never reached this host: " + err.Error()
	}
	want := strconv.Itoa(os.Getuid())
	if strings.TrimSpace(got) != want {
		return StatusFail, fmt.Sprintf("the box's command runs as uid %s, want the caller's %s", strings.TrimSpace(got), want)
	}
	return StatusPass, "the box's command runs as the caller's uid " + want
}

func probeFSHostLocal(_ context.Context, _ *runner, hs runtime.ProtocolInfo) (Status, string) {
	if status, detail, ok := fsHostDeclared(hs); !ok {
		return status, detail
	}
	if !hs.DeclaresLocal() {
		return StatusFail, fmt.Sprintf("fs.host is declared with locality %q; it narrows locality local, and without it gc reads fs.host as undeclared", hs.Locality)
	}
	return StatusPass, "fs.host is declared with locality local"
}

func probeFSHostRunsWhatItCarries(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if status, detail, ok := fsHostDeclared(hs); !ok {
		return status, detail
	}
	dir, err := r.fsHostDir()
	if err != nil {
		return StatusFail, "making the probe's work dir: " + err.Error()
	}
	defer func() { _ = os.RemoveAll(dir) }()
	work, overlay := filepath.Join(dir, "work"), filepath.Join(dir, "overlay")
	for _, d := range []string{work, overlay} {
		if err := os.Mkdir(d, 0o755); err != nil {
			return StatusFail, "making the probe's dirs: " + err.Error()
		}
	}
	if err := os.WriteFile(filepath.Join(overlay, "gc-rpp-fshost-overlay"), []byte("overlay"), 0o644); err != nil {
		return StatusFail, "writing the probe overlay: " + err.Error()
	}
	marker := filepath.Join(work, "gc-rpp-fshost-prestart")

	name := r.nextName()
	cfg := fsHostConfig{WorkDir: work, Command: r.opts.Command, OverlayDir: overlay, PreStart: []string{"touch " + shellquote.Quote(marker)}}
	if status, detail := startFSHostOrFail(ctx, r, name, cfg); status != StatusPass {
		return status, detail
	}
	r.stop(ctx, name)
	if _, err := os.Stat(filepath.Join(work, "gc-rpp-fshost-overlay")); err != nil {
		return StatusFail, "start did not stage the overlay_dir its config carries: " + err.Error()
	}
	if _, err := os.Stat(marker); err != nil {
		return StatusFail, "start did not run the pre_start its config carries: " + err.Error()
	}

	failing := r.nextName()
	res := r.startFSHost(ctx, failing, fsHostConfig{WorkDir: work, Command: r.opts.Command, PreStart: []string{"exit 1"}})
	defer r.stop(ctx, failing)
	switch {
	case res.unsupported:
		return StatusFail, "start returned exit 2; start is a required op"
	case res.err == nil:
		return StatusFail, "start succeeded although its pre_start failed; a failing pre_start must fail start"
	}
	if status, detail := expectRunning(ctx, r, failing, false, "after a start whose pre_start failed"); status != StatusPass {
		return status, detail
	}
	return StatusPass, "start stages and runs what its config carries, and a failing pre_start fails it with no box"
}

func probeFSHostAgentPIDOnHost(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if status, detail, ok := fsHostDeclared(hs); !ok {
		return status, detail
	}
	if status, detail, ok := observeDeclared(hs); !ok {
		return status, detail
	}
	name := r.nextName()
	if status, detail := startFSHostOrFail(ctx, r, name, fsHostConfig{WorkDir: r.opts.WorkDir, Command: r.opts.Command}); status != StatusPass {
		return status, detail
	}
	defer r.stop(ctx, name)
	o, status, detail := presentObserve(ctx, r, name, "running")
	if status != StatusPass {
		return status, detail
	}
	pid, err := strconv.Atoi(o.AgentPID)
	if err != nil || pid <= 0 {
		return StatusFail, fmt.Sprintf("observe reports agent_pid %q, want the agent's host pid", o.AgentPID)
	}
	switch err := syscall.Kill(pid, 0); {
	case errors.Is(err, syscall.ESRCH):
		return StatusFail, fmt.Sprintf("agent_pid %d is no process on this host (a pid from another pid namespace?)", pid)
	case errors.Is(err, syscall.EPERM):
		return StatusFail, fmt.Sprintf("agent_pid %d belongs to another user", pid)
	case err != nil:
		return StatusFail, fmt.Sprintf("agent_pid %d: %v", pid, err)
	}
	return StatusPass, fmt.Sprintf("agent_pid %d is a live host process of the caller's user", pid)
}
