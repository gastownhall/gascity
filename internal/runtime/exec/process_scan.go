package exec

import (
	"errors"
	"fmt"
	"slices"
	"strings"

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

// opSessionIdentityKeys are the session identity gc's own environment
// carries when gc runs in an agent's shell (a gc command an agent runs).
// Every op runs without them: an op is the provider's, never a session's,
// and a server an op starts and leaves running must not read to the scanner
// as that session's leftover, which the orphan sweep would end with the
// session. A session's identity reaches its box in the start config's env.
var opSessionIdentityKeys = []string{"GC_SESSION_ID", "GC_RUNTIME_EPOCH"}

// sessionlessEnviron is gc's environment (execenv.Environ) without
// opSessionIdentityKeys.
func sessionlessEnviron() []string {
	environ := execenv.Environ()
	out := make([]string, 0, len(environ))
	for _, entry := range environ {
		key, _, _ := strings.Cut(entry, "=")
		if slices.Contains(opSessionIdentityKeys, key) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

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
	ids, listErr := p.listedSessionIDs()
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
		name, err := proctable.ProcessEnvValue(root.PID, "GC_SESSION_NAME")
		if err != nil {
			root.LiveUnlisted = true
			errs = append(errs, &proctable.EntryError{PID: root.PID, Err: fmt.Errorf("exec provider %s: reading GC_SESSION_NAME of PID %d: %w", p.script, root.PID, err)})
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
// partial listing returns what it listed.
func (p *Provider) listedSessionIDs() (map[string]string, error) {
	ids := make(map[string]string)
	if p.v2Ready() {
		names, entries, err := p.ListInventory("")
		if err != nil && !runtime.IsPartialListError(err) {
			return nil, err
		}
		for _, name := range names {
			e := entries[name]
			if e.IdentityKnown {
				ids[name] = strings.TrimSpace(e.Identity["GC_SESSION_ID"])
			} else {
				ids[name] = ""
			}
		}
		return ids, err
	}
	names, err := p.ListRunning("")
	if err != nil && !runtime.IsPartialListError(err) {
		return nil, err
	}
	for _, name := range names {
		id, metaErr := p.GetMeta(name, "GC_SESSION_ID")
		if metaErr != nil {
			id = ""
		}
		ids[name] = strings.TrimSpace(id)
	}
	return ids, err
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
