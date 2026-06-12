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
