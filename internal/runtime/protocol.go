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
)

// ProtocolInfo is the parsed `protocol` handshake response. The zero
// value is the contract for executables that do not implement the
// handshake: version 0, no optional capabilities.
type ProtocolInfo struct {
	Version      int      `json:"version"`
	Capabilities []string `json:"capabilities,omitempty"`
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
