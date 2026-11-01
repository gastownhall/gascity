package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	sessiontmux "github.com/gastownhall/gascity/internal/runtime/tmux"
)

const (
	// detachedProbeMetadataKey is a work-bead metadata contract documented in
	// engdocs/architecture/health-patrol.md. Values use tmux:<socket>:<session>.
	detachedProbeMetadataKey    = beadmeta.DetachedMetadataKey
	detachedProbeDefaultTimeout = time.Second
	// detachedHoldLogInterval bounds how often one bead's unobservable-namespace
	// hold is logged: the hold repeats every patrol pass, but a namespace that
	// stays broken must not flood the controller log.
	detachedHoldLogInterval = 5 * time.Minute
)

type detachedProbeStatus string

const (
	detachedProbeAlive   detachedProbeStatus = "alive"
	detachedProbeDead    detachedProbeStatus = "dead"
	detachedProbeError   detachedProbeStatus = "error"
	detachedProbeTimeout detachedProbeStatus = "timeout"
)

type detachedProbeSpec struct {
	Kind    string
	Socket  string
	Session string
}

type detachedProbeResult struct {
	Status detachedProbeStatus
	Spec   detachedProbeSpec
	Err    error
}

var detachedHoldLog = struct {
	sync.Mutex
	lastByBead map[string]time.Time
}{
	lastByBead: make(map[string]time.Time),
}

// logDetachedProbeHold reports a hold on work whose detached namespace stays
// unobservable at most once per detachedHoldLogInterval per bead, so the log
// keeps one diagnostic per stuck namespace instead of one per patrol pass.
func logDetachedProbeHold(id, format string, args ...any) {
	now := time.Now()
	detachedHoldLog.Lock()
	if last, ok := detachedHoldLog.lastByBead[id]; ok && now.Sub(last) < detachedHoldLogInterval {
		detachedHoldLog.Unlock()
		return
	}
	detachedHoldLog.lastByBead[id] = now
	detachedHoldLog.Unlock()
	log.Printf(format, args...)
}

// forgetDetachedProbeHold drops a bead's hold-log throttle once its probe
// decides alive or dead, or its gc.detached is cleared, so the throttle map
// holds only beads still held.
func forgetDetachedProbeHold(id string) {
	detachedHoldLog.Lock()
	defer detachedHoldLog.Unlock()
	delete(detachedHoldLog.lastByBead, id)
}

func resetDetachedProbeHoldLogForTest() {
	detachedHoldLog.Lock()
	defer detachedHoldLog.Unlock()
	detachedHoldLog.lastByBead = make(map[string]time.Time)
}

func parseDetachedProbeSpec(spec string) (detachedProbeSpec, error) {
	parts := strings.SplitN(strings.TrimSpace(spec), ":", 3)
	if len(parts) != 3 {
		return detachedProbeSpec{}, fmt.Errorf("parsing detached probe spec %q: want tmux:<socket>:<session>", spec)
	}
	kind := strings.TrimSpace(parts[0])
	socket := strings.TrimSpace(parts[1])
	session := strings.TrimSpace(parts[2])
	if kind != "tmux" {
		return detachedProbeSpec{}, fmt.Errorf("parsing detached probe spec %q: unsupported kind %q", spec, kind)
	}
	if socket == "" || session == "" {
		return detachedProbeSpec{}, fmt.Errorf("parsing detached probe spec %q: socket and session are required", spec)
	}
	// tmux never names a session with ':' or '.', so such a session could
	// never be listed and would read as dead forever.
	if strings.ContainsAny(session, ":.") {
		return detachedProbeSpec{}, fmt.Errorf("parsing detached probe spec %q: session %q is not a tmux session name", spec, session)
	}
	if strings.Contains(socket, "/") && !filepath.IsAbs(socket) {
		return detachedProbeSpec{}, fmt.Errorf("parsing detached probe spec %q: socket %q is neither a socket name nor an absolute path", spec, socket)
	}
	return detachedProbeSpec{
		Kind:    kind,
		Socket:  socket,
		Session: session,
	}, nil
}

func probeDetachedWork(ctx context.Context, spec string) detachedProbeResult {
	return probeDetachedWorkWithTimeout(ctx, spec, detachedProbeDefaultTimeout)
}

// probeDetachedWorkWithTimeout projects tmux.ProbeSession onto the probe
// statuses: only an attested-dead session is dead; anything the probe could
// not observe is timeout when the probe's deadline cut it short, else error.
func probeDetachedWorkWithTimeout(ctx context.Context, spec string, timeout time.Duration) detachedProbeResult {
	parsed, err := parseDetachedProbeSpec(spec)
	if err != nil {
		return detachedProbeResult{Status: detachedProbeError, Err: err}
	}
	if timeout <= 0 {
		timeout = detachedProbeDefaultTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	presence, err := sessiontmux.ProbeSession(probeCtx, parsed.Socket, parsed.Session)
	switch presence {
	case sessiontmux.PresenceAlive:
		return detachedProbeResult{Status: detachedProbeAlive, Spec: parsed}
	case sessiontmux.PresenceDead:
		return detachedProbeResult{Status: detachedProbeDead, Spec: parsed}
	}
	if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
		return detachedProbeResult{Status: detachedProbeTimeout, Spec: parsed, Err: err}
	}
	return detachedProbeResult{Status: detachedProbeError, Spec: parsed, Err: err}
}
