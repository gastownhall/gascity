package proxyendpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// BdDefaultIdleTimeout is the window bd's unit-of-work provider substitutes
// when the sidecar carries no idle timeout (beads
// internal/storage/uow/dolt_sql_provider.go defaultProxyIdleTimeout).
//
// It is the value that makes an operator-initialized proxied scope a different
// animal from a gc-initialized one: after this much quiet bd retires the proxy
// AND its Dolt child, so a long-lived gc connection to such a scope would hold
// open a process bd has decided to stop.
const BdDefaultIdleTimeout = 30 * time.Second

// IdleTimeoutNever is what bd persists for "stay resident": a negative
// duration (beads proxy.IdleTimeoutNever is -1ns). Any negative value means
// never, so callers compare with < 0 rather than against this constant.
const IdleTimeoutNever = -1 * time.Nanosecond

// IdleKind classifies a proxy's idle behavior. It is an enum and not a bool
// because "we could not tell" is a real third answer: an argv gc could not read
// on a scope with no sidecar leaves the question open, and a caller that has to
// choose between a resident pool and a per-operation open must not read that as
// "never".
type IdleKind int

// Idle kinds.
const (
	// IdleUnknown means no source answered. Callers must treat it as the
	// conservative case (a finite window), never as never.
	IdleUnknown IdleKind = iota
	// IdleNever means the proxy has no idle timeout and stays resident until
	// something stops it.
	IdleNever
	// IdleFinite means the proxy retires itself and its Dolt child after
	// Timeout of quiet.
	IdleFinite
)

// String renders the kind for a diagnostic.
func (k IdleKind) String() string {
	switch k {
	case IdleNever:
		return "never"
	case IdleFinite:
		return "finite"
	default:
		return "unknown"
	}
}

// IdleSource records which evidence produced a policy. It is reported because
// the two sources can legitimately disagree — an operator can edit the sidecar
// under a live proxy, and only the argv describes the process that is actually
// running — and an operator debugging an unexpected retirement needs to know
// which one gc believed.
type IdleSource int

// Idle policy sources.
const (
	// IdleSourceNone means nothing answered.
	IdleSourceNone IdleSource = iota
	// IdleSourceArgv is the live supervisor's own --idle-timeout, the only
	// source that describes the running process.
	IdleSourceArgv
	// IdleSourceSidecar is an explicit idle_timeout in the scope's sidecar.
	IdleSourceSidecar
	// IdleSourceBdDefault is bd's substituted default for an absent or zero
	// sidecar value.
	IdleSourceBdDefault
)

// String renders the source for a diagnostic.
func (s IdleSource) String() string {
	switch s {
	case IdleSourceArgv:
		return "argv"
	case IdleSourceSidecar:
		return "sidecar"
	case IdleSourceBdDefault:
		return "bd-default"
	default:
		return "none"
	}
}

// IdlePolicy is a resolved idle rule with the evidence it came from.
type IdlePolicy struct {
	Kind    IdleKind
	Timeout time.Duration
	Source  IdleSource
}

// String renders the policy the way the diagnostic and doctor report it:
// "never(argv)", "finite(30s, bd-default)", "unknown".
func (p IdlePolicy) String() string {
	switch p.Kind {
	case IdleNever:
		return fmt.Sprintf("never(%s)", p.Source)
	case IdleFinite:
		return fmt.Sprintf("finite(%s, %s)", p.Timeout, p.Source)
	default:
		return IdleUnknown.String()
	}
}

// Known reports whether any source answered.
func (p IdlePolicy) Known() bool { return p.Kind != IdleUnknown }

// Sidecar is the subset of bd's .beads/proxied_server_client_info.json gc
// reads. It is a read contract like the record: gc never writes this file, and
// `bd init` is the only thing that should.
//
// IdleTimeout is a POINTER on purpose. bd's own struct tags the field
// `json:"idle_timeout,omitempty"`, so a zero is never written and reads back
// indistinguishable from absent — which is exactly the distinction the doctor
// check needs, because "bd wrote nothing" and "somebody wrote 0" are different
// facts about a scope even though bd's provider treats them the same way.
type Sidecar struct {
	RootPath    string         `json:"root_path,omitempty"`
	ConfigPath  string         `json:"config_path,omitempty"`
	LogPath     string         `json:"log_path,omitempty"`
	Port        int            `json:"port,omitempty"`
	IdleTimeout *time.Duration `json:"idle_timeout,omitempty"`
	// Present is false when the scope has no sidecar at all, which is a
	// different state from a sidecar with no idle_timeout in it.
	Present bool `json:"-"`
}

// SidecarPath returns the sidecar path inside a scope's .beads directory.
func SidecarPath(beadsDir string) string { return filepath.Join(beadsDir, SidecarFileName) }

// ReadSidecar decodes a scope's sidecar. An absent file is not an error: it
// returns a zero Sidecar with Present false, because a direct scope legitimately
// has none and every caller's next question is "is this scope proxied at all".
func ReadSidecar(beadsDir string) (Sidecar, error) {
	var sidecar Sidecar
	data, err := os.ReadFile(SidecarPath(beadsDir)) // #nosec G304 -- beadsDir is a resolved scope .beads directory
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return sidecar, nil
		}
		return sidecar, fmt.Errorf("read %s: %w", SidecarPath(beadsDir), err)
	}
	if err := json.Unmarshal(data, &sidecar); err != nil {
		return Sidecar{}, fmt.Errorf("parse %s: %w", SidecarPath(beadsDir), err)
	}
	sidecar.Present = true
	return sidecar, nil
}

// ResolvedRootPath resolves the sidecar's root_path the way bd does: absolute
// as written, relative against the scope's .beads directory, empty when unset.
func (s Sidecar) ResolvedRootPath(beadsDir string) string {
	if s.RootPath == "" {
		return ""
	}
	if filepath.IsAbs(s.RootPath) {
		return filepath.Clean(s.RootPath)
	}
	return filepath.Join(beadsDir, s.RootPath)
}

// ExplicitIdleNever reports whether the sidecar states, in writing, that its
// proxy has no idle timeout.
//
// This is the property a gc-owned scope must have and the one the doctor check
// asserts. gc's provider script passes `--proxied-server-idle-timeout 0` at
// init, which bd maps to IdleTimeoutNever before persisting, so a gc-owned
// scope's sidecar reads back negative. An ABSENT key on such a scope is not the
// same scope: bd's provider would substitute 30s and retire the proxy under a
// resident reader.
func (s Sidecar) ExplicitIdleNever() bool {
	return s.Present && s.IdleTimeout != nil && *s.IdleTimeout < 0
}

// IdlePolicy is the policy the sidecar alone implies, with bd's semantics:
// absent or zero is bd's 30s default, negative is never, positive is that
// window.
//
// Absent and zero deliberately produce the same answer with different sources,
// so a diagnostic can distinguish "bd wrote nothing and its provider will
// default" from "this scope asked for the default in writing" without either
// being treated as never.
func (s Sidecar) IdlePolicy() IdlePolicy {
	if !s.Present {
		return IdlePolicy{Kind: IdleFinite, Timeout: BdDefaultIdleTimeout, Source: IdleSourceBdDefault}
	}
	switch {
	case s.IdleTimeout == nil || *s.IdleTimeout == 0:
		return IdlePolicy{Kind: IdleFinite, Timeout: BdDefaultIdleTimeout, Source: IdleSourceBdDefault}
	case *s.IdleTimeout < 0:
		return IdlePolicy{Kind: IdleNever, Source: IdleSourceSidecar}
	default:
		return IdlePolicy{Kind: IdleFinite, Timeout: *s.IdleTimeout, Source: IdleSourceSidecar}
	}
}

// ResolveIdlePolicy picks the policy that describes the proxy gc would actually
// talk to. The live supervisor's argv wins whenever it answered, because it is
// the only source that describes the running process: bd passes the effective
// value into the fork-exec, and an operator who edited the sidecar afterwards
// has changed the NEXT proxy, not this one.
func ResolveIdlePolicy(sidecar Sidecar, argv IdlePolicy) IdlePolicy {
	if argv.Known() {
		return argv
	}
	return sidecar.IdlePolicy()
}

// idlePolicyFromDuration maps a duration in bd's argv spelling onto a policy.
// bd renders IdleTimeoutNever as a negative duration and everything else as
// itself, and the supervisor disables its idle watcher for any value <= 0
// (beads dbproxy/proxy/server.go), so zero is never here rather than the
// provider's default: the provider's substitution happens before the fork, so
// an argv that says zero is a proxy that will not retire.
func idlePolicyFromDuration(d time.Duration, source IdleSource) IdlePolicy {
	if d <= 0 {
		return IdlePolicy{Kind: IdleNever, Source: source}
	}
	return IdlePolicy{Kind: IdleFinite, Timeout: d, Source: source}
}
