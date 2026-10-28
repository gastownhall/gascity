package main

import (
	"context"
	"errors"
	"fmt"
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
	detachedProbeErrorThreshold = 3
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

var detachedProbeErrorCounts = struct {
	sync.Mutex
	byBead map[string]int
}{
	byBead: make(map[string]int),
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
	// The probe classifies the session by an exact list-sessions line, so the
	// session must be a bare tmux session name: tmux splits targets on ':' and
	// windows on '.', and neither can name exactly one session.
	if strings.ContainsAny(session, ":.") {
		return detachedProbeSpec{}, fmt.Errorf("parsing detached probe spec %q: session %q must be a bare tmux session name without ':' or '.'", spec, session)
	}
	// The socket is a -L name or a -S path; a relative path with a slash is
	// neither and cannot resolve to the path tmux itself uses.
	if !filepath.IsAbs(socket) && strings.Contains(socket, "/") {
		return detachedProbeSpec{}, fmt.Errorf("parsing detached probe spec %q: socket %q must be a bare socket name or an absolute path", spec, socket)
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

// probeDetachedWorkWithTimeout evaluates one detached probe spec through the
// attested tmux session probe: alive, attested dead, or unobservable. An
// unobservable namespace is an error — a timeout when the probe's own bound
// expired — because reachability is never death. The timeout parameter is the
// probe's own bound, separate from the caller's ctx.
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
	default:
		if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
			return detachedProbeResult{Status: detachedProbeTimeout, Spec: parsed, Err: err}
		}
		return detachedProbeResult{Status: detachedProbeError, Spec: parsed, Err: err}
	}
}

func incrementDetachedProbeErrorCount(id string) int {
	detachedProbeErrorCounts.Lock()
	defer detachedProbeErrorCounts.Unlock()
	if detachedProbeErrorCounts.byBead == nil {
		detachedProbeErrorCounts.byBead = make(map[string]int)
	}
	detachedProbeErrorCounts.byBead[id]++
	return detachedProbeErrorCounts.byBead[id]
}

func clearDetachedProbeErrorCount(id string) {
	detachedProbeErrorCounts.Lock()
	defer detachedProbeErrorCounts.Unlock()
	delete(detachedProbeErrorCounts.byBead, id)
}

func resetDetachedProbeErrorCountsForTest() {
	detachedProbeErrorCounts.Lock()
	defer detachedProbeErrorCounts.Unlock()
	detachedProbeErrorCounts.byBead = make(map[string]int)
}
