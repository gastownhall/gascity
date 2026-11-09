package runtime

import "fmt"

// RPP (Runtime Provider Protocol) is the versioned wire contract spoken
// to out-of-process runtime providers. Version 0 is the exec contract
// (one process invocation per op, payloads on stdin/stdout, exit 2 =
// unknown op). The protocol description scripts are written against is
// docs/reference/exec-session-provider.md; the behavior ledger is
// internal/runtime/REQUIREMENTS.md (RUNTIME-RPP rows).

// ProtocolVersion0 is the current Runtime Provider Protocol version.
const ProtocolVersion0 = 0

// Capability strings an executable may declare in its `protocol`
// handshake. Unknown strings are ignored for forward compatibility.
const (
	// ProtocolCapabilityReportAttachment declares that the executable
	// implements `is-attached <name>` with meaningful results, enabling
	// ProviderCapabilities.CanReportAttachment.
	ProtocolCapabilityReportAttachment = "report-attachment"
	// ProtocolCapabilityReportActivity declares that
	// `get-last-activity <name>` returns meaningful results, enabling
	// ProviderCapabilities.CanReportActivity.
	ProtocolCapabilityReportActivity = "report-activity"
	// ProtocolCapabilityConnectionExec declares that the executable implements
	// the `exec` connection op (RPP-CONN-001) with the op's process exit code
	// carrying the in-box command's exit code. The controller uses this to read
	// an exec-op exit of 2 as the command's own exit code rather than the RPP
	// "unknown op" sentinel (ErrExecUnsupported) — disambiguating the overloaded
	// exit 2 at the exec-connection seam. exec stays optional; an executable that
	// omits this capability is driven via the dedicated ops (the fallback path).
	ProtocolCapabilityConnectionExec = "proc.exec"
	// ProtocolCapabilityInputFence declares that the box's tmux can take
	// fenced input (RUNTIME-CONTRACT-009): tmux 3.0 or later, the `main`
	// session present when `start` returns, and `sh`. Declared with
	// proc.exec, it makes gc publish each agent pane's input fence and guard
	// fenced input over the exec connection (InputFencingAtomic); without it
	// the pack reports InputFencingNone, and starts and launches touch no
	// fence.
	ProtocolCapabilityInputFence = "input-fence"
	// ProtocolCapabilityIdleBoundary declares that gc may read the agent's
	// idle boundary from the box's tmux: the agent runs in the in-box tmux
	// session `main`, readable with `capture-pane` over the exec connection.
	// Declared with proc.exec, report-activity and report-attachment, it gives
	// the pack an idle boundary (WaitForIdle, the ready-prompt publication and
	// the idle-proof budget) and full idle sleep; without it those three alone
	// report timed-only sleep, because nothing else proves the box runs tmux.
	//
	// The name states the guarantee gc relies on (it may read the agent's idle
	// boundary), not the mechanism, so it keeps its meaning once carriers other
	// than tmux exist. When a pack can select its carrier (gh#8903), this
	// definition widens from
	// "tmux in the box" to "the carrier the pack declares can answer a Peek",
	// and RPP-CONN-002 checks the declared carrier's liveness and read; tmux's
	// check stays the one it runs today. A non-tmux carrier reuses this token
	// rather than adding one.
	ProtocolCapabilityIdleBoundary = "idle-boundary"
	// The proc.* / tty.* tokens below form the connection-plane capability
	// family — a dotted namespace parallel to the env.* family, distinct from
	// the flat session-control tokens above. They are reserved now and gain
	// their ops + capability-gated conformance entries with the connection
	// rewrite; nothing consumes them yet.
	//
	// ProtocolCapabilityProcStream declares that the executable implements the
	// persistent bidirectional `stream` connection op (ACP over a stream, or
	// tmux pipe-pane output), enabling ProviderCapabilities.CanStream.
	ProtocolCapabilityProcStream = "proc.stream"
	// ProtocolCapabilityTTYAttach declares that the executable implements an
	// interactive PTY `attach` connection op, enabling
	// ProviderCapabilities.CanAttachTTY.
	ProtocolCapabilityTTYAttach = "tty.attach"
	// ProtocolCapabilityProvision declares that the executable implements the
	// `provision` op — a box-without-agent `start` that creates+stages the box
	// and runs pre_start but does NOT launch the agent (un-weld B3b). With it
	// declared, the controller provisions via `provision` and then launches the
	// agent itself by exec-ing `tmux new-session` over the `exec` op (so it also
	// requires proc.exec); without it, the controller uses the welded `start` op
	// (compat). It gates TransportCapabilities.SeparableLaunch.
	ProtocolCapabilityProvision = "proc.provision"
	// ProtocolCapabilityObserveV1 declares the exec observe contract
	// reconciler v2 reads (PAR-EXEC): the `observe`, `inventory` and
	// `kill-object` ops, `fresh_only` honored on `start` and `provision`, and
	// exit 2 from a declared op read as a failure, never as the unknown-op
	// no-op. With report-attachment it makes an exec runtime v2-ready.
	ProtocolCapabilityObserveV1 = "observe.v1"
	// ProtocolCapabilityServerLifecycle declares that the executable owns a
	// server its boxes run under and implements the `server-stop` op, which
	// gc runs after it stops the city's sessions (`gc stop`, the
	// controller's shutdown) to end that server. It exits 0 only when the
	// server is gone and no session the executable started for the city
	// remains, and 0 again when there is no server (idempotent). With it the
	// exec provider's TeardownServer runs `server-stop`; without it,
	// TeardownServer sends nothing.
	ProtocolCapabilityServerLifecycle = "server-lifecycle"
	// ProtocolCapabilityFSHost declares that the executable's boxes share the
	// controller's host filesystem: they run on the controller's host, as the
	// controller's user, with the controller's view of the filesystem, so the
	// work_dir, HOME and transcript paths gc holds name the same files inside
	// the box. It narrows [ProtocolLocalityLocal], which says only that boxes
	// run on the controller's host (a container on that host is local too, and
	// must not declare fs.host unless it shares that view and user). It is
	// read only through [ProtocolInfo.SharesHostFS], never on its own: with
	// it, gc stages the work dir and runs pre_start and session_setup on the
	// host itself and leaves that work out of the start config.
	ProtocolCapabilityFSHost = "fs.host"
)

// ProtocolLocalityLocal is the handshake locality of an executable whose
// boxes run on the controller's host. Any other value, or none, reads as
// remote.
const ProtocolLocalityLocal = "local"

// ProtocolInfo is the parsed `protocol` handshake response. The zero
// value is the contract for executables that do not implement the
// handshake: version 0, no optional capabilities.
type ProtocolInfo struct {
	Version      int      `json:"version"`
	Capabilities []string `json:"capabilities,omitempty"`
	// Locality is where the executable's boxes run, when it says:
	// [ProtocolLocalityLocal], or anything else for remote.
	Locality string `json:"locality,omitempty"`
}

// DeclaresLocal reports whether the handshake declared its boxes local to
// the controller's host. An absent or unknown locality is remote.
func (pi ProtocolInfo) DeclaresLocal() bool {
	return pi.Locality == ProtocolLocalityLocal
}

// SharesHostFS reports whether the handshake declared that its boxes share
// the controller's host filesystem and user: locality=local narrowed by
// [ProtocolCapabilityFSHost]. It is the one reader of that token, so
// locality and the token are never checked apart; fs.host without
// locality=local reads as undeclared.
func (pi ProtocolInfo) SharesHostFS() bool {
	return pi.DeclaresLocal() && pi.Has(ProtocolCapabilityFSHost)
}

// Has reports whether the handshake declared the given capability.
func (pi ProtocolInfo) Has(capability string) bool {
	for _, c := range pi.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// Validate checks structural invariants of a parsed handshake.
func (pi ProtocolInfo) Validate() error {
	if pi.Version < 0 {
		return fmt.Errorf("protocol handshake: version %d is negative", pi.Version)
	}
	return nil
}
