// Package runtimecapability is the environment-plane conformance suite for
// Runtime Provider Protocol executables — the sibling of runtimecontract one
// layer up. Where runtimecontract proves the session control plane (a runtime
// can be started/stopped/observed), runtimecapability proves the session is a
// viable home for a Gas City agent: the workspace was materialized, the
// tooling is installed, the identity/env was injected.
//
// The model:
//
//   - A runtime DECLARES the environment guarantees it provides via the RPP
//     protocol handshake capabilities, using the namespaced env.* family
//     (env.workspace, env.tooling, env.identity, …).
//   - For each DECLARED capability, [Run] executes a probe that verifies the
//     guarantee by running commands inside a real session (via the runtime's
//     exec op) and inspecting the result — so a probe tests the runtime's
//     actual wiring, not a self-report. Undeclared capabilities SKIP.
//   - Declaring a capability you do not satisfy fails its probe (negative
//     gating, proven by TestEveryCapabilityIsGated): "declared" is not
//     "guaranteed".
//
// Probing the session interior requires an RPP exec op the protocol suite
// does not mandate: `<exe> exec <name>` with the command on stdin, combined
// output on stdout, and the op's process exit code equal to the command's
// exit code (exit 2 reserved for "exec op not implemented"). Any runtime that
// declares an env.* capability must implement exec, or its capability probes
// cannot pass.
package runtimecapability

// Code is an RPP environment capability string (the handshake token).
type Code string

// Environment capability codes (the env.* handshake family).
const (
	CapWorkspace Code = "env.workspace"
	CapTooling   Code = "env.tooling"
	CapIdentity  Code = "env.identity"
	CapLedger    Code = "env.ledger"
)

// Capability is one environment guarantee a runtime may declare and that
// conformance can verify.
type Capability struct {
	// Code is the handshake token (e.g. "env.workspace").
	Code Code
	// Title is a one-line description of the guarantee.
	Title string
}

// catalog is the authoritative, ordered capability list. Append-only.
var catalog = []Capability{
	{CapWorkspace, "the start-config work_dir is materialized in the session (file transfer in)"},
	{CapTooling, "the agent toolchain (gc, bd, git, …) is installed and runnable in the session"},
	{CapIdentity, "the session identity/env (GC_* vars, run-as user) is injected"},
	{CapLedger, "the session's bd can reach the work ledger (the gc beads API) — transport (tunnel) is the runtime's concern"},
}

// Catalog returns the capability list in probe order (a copy).
func Catalog() []Capability {
	out := make([]Capability, len(catalog))
	copy(out, catalog)
	return out
}
