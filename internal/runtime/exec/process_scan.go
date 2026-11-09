package exec

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// This file is the exec provider's process-table scanner. A pack whose
// boxes share the controller's host and user (sharesHostFS) runs its
// agents as processes of this host, so gc's orphan sweep and a start's
// orphan kill can find a session's leftover processes, the ones that left
// the agent's process group and outlived its terminal, by the
// GC_SESSION_ID they carry, exactly as they do for tmux and subprocess
// sessions. Every exec provider implements the scanner, but only a pack
// sharing the host can scan (runtime.ConditionalProcessTableScanner): for
// any other pack, runtime.AsProcessTableScanner reads it as having none,
// and its methods find nothing and terminate nothing.

var (
	_ runtime.ProcessTableScanner            = (*Provider)(nil)
	_ runtime.ConditionalProcessTableScanner = (*Provider)(nil)
	_ runtime.ProcessTableScanner            = (*seamBackedProvider)(nil)
	_ runtime.ConditionalProcessTableScanner = (*seamBackedProvider)(nil)
)

// sessionlessEnviron is gc's environment (execenv.Environ) without the
// session identity it carries when gc runs in an agent's shell (a gc command
// an agent runs): execenv.WithoutSessionIdentity, the list gc's other
// detached processes are started without. Every op runs with it: an op is
// the provider's, never a session's, and a server an op starts and leaves
// running must not read to the scanner as that session's leftover, which
// the orphan sweep would end with the session, nor carry its instance token
// into a pack's own identity scans. A session's identity reaches its box in
// the start config's env.
func sessionlessEnviron() []string {
	return execenv.WithoutSessionIdentity(execenv.Environ())
}

// processEnvValue reads a scanned process's environment variable; a test
// fails the read.
var processEnvValue = proctable.ProcessEnvValue

// defaultScanListingTimeout bounds the listing a scan that found roots
// reads to track them (trackRoots). The orphan sweep scans on every
// controller tick, and a pack's listing (an inventory that reads every
// agent's screen) can take up to the op timeout; past this bound the
// listing fails, which is a whole-scan failure: the sweep then reaps only
// the leftovers of closed sessions, and a start refuses rather than reap.
const defaultScanListingTimeout = 5 * time.Second

// CanScanProcessTable implements [runtime.ConditionalProcessTableScanner]: a
// pack can be scanned only when its boxes share this host and user. A pack
// whose boxes run elsewhere, or in a container of this host, runs no
// process this host's table can attribute to it.
func (p *Provider) CanScanProcessTable() bool {
	return p.sharesHostFS()
}

// FindRuntimesBySessionID implements [runtime.ProcessTableScanner] with the
// host's process-table scan (proctable.ScanBySessionID). A root is tracked
// when a box the pack lists carries its session id. A root the listing
// cannot rule out (the listing failed or was partial, or a listed box's
// identity is unreadable and the root names no other session) is
// LiveUnlisted and the failure is returned, so no caller reaps a live
// box's process on a listing it could not read. A pack that cannot scan
// finds nothing.
func (p *Provider) FindRuntimesBySessionID(id string) ([]runtime.LiveRuntime, error) {
	if !p.sharesHostFS() {
		return nil, nil
	}
	found, scanErr := proctable.ScanBySessionID(id)
	if len(found) == 0 {
		return found, scanErr
	}
	return found, errors.Join(scanErr, p.trackRoots(found))
}

// TerminateRuntime implements [runtime.ProcessTableScanner]: it ends a
// scanned root only while the pid still carries the root's session id and
// start identity (proctable.KillByPIDIdentity), so it never signals a
// process outside the session, a recycled pid included; a pid that no
// longer does is gone, which is success. It refuses a root without a
// session id or start identity to bind the kill to, and every root of a
// pack that cannot scan.
func (p *Provider) TerminateRuntime(r runtime.LiveRuntime) error {
	if !p.sharesHostFS() {
		return fmt.Errorf("exec provider %s: refusing to terminate PID %d for session %s: the pack's boxes do not share this host", p.script, r.PID, r.SessionID)
	}
	if r.PID <= 1 {
		return fmt.Errorf("exec provider %s: invalid PID %d for session %s", p.script, r.PID, r.SessionID)
	}
	if r.SessionID == "" || r.StartIdentity == "" {
		return fmt.Errorf("exec provider %s: refusing to terminate PID %d: no session id and start identity to bind the kill to", p.script, r.PID)
	}
	sessionID, err := proctable.ProcessEnvValue(r.PID, "GC_SESSION_ID")
	if err != nil {
		return fmt.Errorf("exec provider %s: reading the session of PID %d: %w", p.script, r.PID, err)
	}
	if sessionID != r.SessionID {
		return nil
	}
	if err := proctable.KillByPIDIdentity(r.PID, r.StartIdentity); err != nil {
		return fmt.Errorf("exec provider %s: terminate runtime PID %d for session %s: %w", p.script, r.PID, r.SessionID, err)
	}
	return nil
}

// trackRoots marks each root of found that a box the pack lists carries,
// and each root the listing cannot rule out as LiveUnlisted, and returns
// the failures that left a root undecided.
func (p *Provider) trackRoots(found []runtime.LiveRuntime) error {
	timeout := p.scanListingTimeout
	if timeout <= 0 {
		timeout = defaultScanListingTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ids, listErr := p.listedSessionIDs(ctx)
	complete := listErr == nil
	var errs []error
	if listErr != nil {
		errs = append(errs, fmt.Errorf("exec provider %s: tracking: %w", p.script, listErr))
	}
	byID := make(map[string]string, len(ids))
	var unattributed []string
	for name, id := range ids {
		if id == "" {
			unattributed = append(unattributed, name)
			continue
		}
		byID[id] = name
	}
	slices.Sort(unattributed)
	undecided := false
	for i := range found {
		root := &found[i]
		if name, ok := byID[root.SessionID]; ok {
			root.IsTracked, root.ProviderName = true, name
			continue
		}
		if complete && len(unattributed) == 0 {
			continue
		}
		// The listing cannot rule the root out by its session id: judge it
		// by the session name its environment carries, as every box's does.
		name, err := processEnvValue(root.PID, "GC_SESSION_NAME")
		if err != nil {
			// The root may be any listed box's: undecided, as a root naming
			// no session is, so tracking reads unknown (a whole-scan
			// failure), not as one unreadable entry.
			root.LiveUnlisted = true
			errs = append(errs, fmt.Errorf("exec provider %s: tracking: reading GC_SESSION_NAME of PID %d: %w", p.script, root.PID, err))
			continue
		}
		if slices.Contains(unattributed, name) {
			root.IsTracked, root.ProviderName = true, name
			continue
		}
		if !complete || (name == "" && len(unattributed) > 0) {
			root.LiveUnlisted = true
			undecided = undecided || complete
		}
	}
	if undecided {
		errs = append(errs, fmt.Errorf("exec provider %s: tracking: listed session(s) %s report no GC_SESSION_ID, and a root naming no session may be theirs", p.script, strings.Join(unattributed, ", ")))
	}
	return errors.Join(errs...)
}

// listedSessionIDs returns the GC_SESSION_ID of every box the pack lists,
// "" for one whose identity it cannot read, with the listing's error: a
// partial listing returns what it listed. Every op it runs is bounded by
// ctx, and a listing ctx ended before it finished is a failure.
func (p *Provider) listedSessionIDs(ctx context.Context) (map[string]string, error) {
	ids := make(map[string]string)
	if p.v2Ready() {
		l, err := p.inventory(ctx)
		if err != nil {
			return nil, err
		}
		for _, name := range l.names {
			if e, ok := l.entries[name]; ok && e.IdentityKnown {
				ids[name] = strings.TrimSpace(e.Identity["GC_SESSION_ID"])
			} else {
				ids[name] = ""
			}
		}
		if !l.complete {
			return ids, &runtime.PartialListError{Err: errors.New("exec provider " + p.script + " inventory: the listing is incomplete")}
		}
		return ids, nil
	}
	out, err := p.runWithContext(ctx, p.timeout, nil, "list-running", "")
	if err != nil {
		return nil, err
	}
	for _, name := range strings.Split(out, "\n") {
		if name == "" {
			continue
		}
		id, metaErr := p.runWithContext(ctx, p.timeout, nil, "get-meta", name, "GC_SESSION_ID")
		if metaErr != nil {
			id = ""
		}
		ids[name] = strings.TrimSpace(id)
	}
	if err := ctx.Err(); err != nil {
		return ids, fmt.Errorf("listing sessions: %w", err)
	}
	return ids, nil
}

// CanScanProcessTable passes the underlying provider's answer through.
func (s *seamBackedProvider) CanScanProcessTable() bool {
	return s.raw.CanScanProcessTable()
}

// FindRuntimesBySessionID passes through to the underlying provider's scan
// (non-seam).
func (s *seamBackedProvider) FindRuntimesBySessionID(id string) ([]runtime.LiveRuntime, error) {
	return s.raw.FindRuntimesBySessionID(id)
}

// TerminateRuntime passes through to the underlying provider (non-seam).
func (s *seamBackedProvider) TerminateRuntime(r runtime.LiveRuntime) error {
	return s.raw.TerminateRuntime(r)
}
