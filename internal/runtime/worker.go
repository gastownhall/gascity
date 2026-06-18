package runtime

import (
	"context"
	"time"
)

// This file is the de-conflation facade: the typed seams the design
// (worker-runtime-transport-v0.md) splits today's monolithic Provider into —
// WHERE (Runtime + Place), HOW (Transport + Attachment) — plus the MetaStore
// seam and the WorkerSpec atom. It is interface-only: the existing [Provider]
// interface and all ~105 call sites are untouched; providers implement these via
// thin delegation in their seams.go, and the cut-over (routing call sites through
// the seams) is a later phase.
//
// The contract is deliberately limited to LOAD-BEARING surface — every method
// here has a real, distinct implementation in some provider. Speculative surface
// with no implementation (a stream/PTY Place extension, a transcript-history
// read, a per-place env accessor) is intentionally NOT declared here; it will be
// added alongside the implementation that needs it, not before.
//
// What the rpp-slim carrier work already pre-paid (REUSED, not net-new):
//   - [ExecProvider].Exec → Place.Exec
//   - [Carrier] (5 verbs) → Attachment.{Peek,Nudge,SendKeys,Interrupt,ClearScrollback}
//   - [ProviderCapabilities].{CanReportActivity,CanReportAttachment}
//     → PlaceCapabilities / TransportCapabilities

// WorkerSpec is the de-conflated worker definition: the five independent axes
// plus the run payload (§4). It is the shared atom the whole stack resolves to —
// a controller plan step resolves to a WorkerSpec (§12). It is forward-declared:
// nothing consumes it yet (the Resolver tail will), but it is the design's atom.
type WorkerSpec struct {
	Harness   string // the agent CLI / harness ("claude-code", "codex", ...)
	Model     string // the single model request label ("opus-4.8")
	Upstream  string // who serves+resolves the model ("anthropic", "bedrock", "byok:acme")
	Runtime   string // WHERE it runs ("local", "k8s", "ssh:user@host", "daytona", ...)
	Transport string // HOW gc drives it ("tmux", "acp")

	WorkDir string
	Prompt  string
	Env     map[string]string
}

// ProvisionRequest is the WHERE-half input to [Runtime.Provision] (the
// provisioning-relevant subset: workdir, env, image/snapshot — carried via
// Config during the migration so the welded Start can be split without changing
// the hashed inputs).
type ProvisionRequest struct {
	Config Config
}

// ExecRequest is the input to Place.Exec. It is a thin shape over
// [ExecProvider.Exec] — same bytes, struct-wrapped — so the carrier work is
// reused verbatim.
type ExecRequest struct {
	Argv  []string
	Stdin []byte // optional; fed to the remote command (e.g. a setup script)
}

// ExecResult is the output of Place.Exec: combined output bytes and exit code.
type ExecResult struct {
	Output []byte
	Code   int
}

// LaunchSpec is the HOW-half input to [Transport.Launch] (the command + startup
// hints + the in-box session target, e.g. "main").
type LaunchSpec struct {
	Config Config
	Target string
}

// PlaceCapabilities is the runtime/box half of today's [ProviderCapabilities]
// (§11): ReportActivity says get-last-activity is meaningful. Reported by
// [Runtime.Capabilities].
type PlaceCapabilities struct {
	ReportActivity bool // get-last-activity is meaningful
}

// TransportCapabilities is the HOW half of today's [ProviderCapabilities] (§11):
// ReportAttachment says is-attached is meaningful. Reported by
// [Transport.Capabilities].
type TransportCapabilities struct {
	ReportAttachment bool // is-attached is meaningful
	// SeparableLaunch reports whether Provision creates the box WITHOUT launching
	// the agent, so the agent is launched separately by Transport.Launch. When
	// false (the default, and every welded provider — tmux/ssh/k8s and welded exec
	// packs), Provision already launches the agent and Launch is the relaunch-only
	// capability, so a normal Start must NOT call Launch. When true (an exec pack
	// that declares proc.provision), a normal Start must call Provision THEN
	// Launch. See seamProvider.Start and the un-weld design (B3b).
	SeparableLaunch bool
}

// LiveObservation folds the three liveness reads — ProcessAlive, IsAttached,
// GetLastActivity — into one Attachment.Observe (§11).
type LiveObservation struct {
	ProcessAlive bool
	Attached     bool
	LastActivity time.Time
}

// Runtime is the WHERE axis: it provisions and lists boxes. Its bodies reuse the
// providers' existing lifecycle. (§11: Start's where-half, ListRunning, the
// env.* half of Capabilities.)
type Runtime interface {
	// Provision creates (or adopts) the box for name and returns a Place. (←Start)
	Provision(ctx context.Context, name string, req ProvisionRequest) (Place, error)
	// Open re-resolves an existing box by name without creating it. Net-new;
	// ssh/exec packs are already stateless-by-name so this is cheap there.
	Open(ctx context.Context, name string) (Place, bool, error)
	// List returns the names of running boxes with the given prefix. (←ListRunning)
	List(ctx context.Context, prefix string) ([]string, error)
	Capabilities() PlaceCapabilities
}

// Place is one provisioned environment — the connection to a box. Exec reuses
// [ExecProvider] verbatim; the rest are the where-half of Stop/CopyTo/IsRunning.
type Place interface {
	Exec(ctx context.Context, req ExecRequest) (ExecResult, error) // ←ExecProvider.Exec
	Stage(ctx context.Context, files []CopyEntry) error            // ←CopyTo + overlay staging
	IsRunning(ctx context.Context) (bool, error)                   // ←IsRunning
	Teardown(ctx context.Context) error                            // ←Stop (where-half)
}

// MetaStore is the per-session key-value store today's [Provider] carries via
// SetMeta/GetMeta/RemoveMeta: session identity (GC_SESSION_ID, GC_PROVIDER),
// lifecycle flags (suspended, drained, GC_ALIAS, GC_INSTANCE_TOKEN), and the
// config fingerprint. It has no natural home among Runtime/Place/Transport/
// Attachment — the values are box ground-truth, read back to reconcile against
// controller belief — so during the migration it stays a DISTINCT optional seam,
// implemented alongside Runtime by providers that own box-side meta. This may be
// retired later if its consumers move to the controller's state seam; until then
// the compat layer delegates SetMeta/GetMeta/RemoveMeta straight to it.
type MetaStore interface {
	SetMeta(name, key, value string) error
	GetMeta(name, key string) (string, error)
	RemoveMeta(name, key string) error
}

// Transport is the HOW axis: it launches the agent over a Place and yields an
// Attachment. The tmux Transport's body is the [Carrier] over Place.Exec; the
// t3/acp Transports drive their own protocol connections. (§11: Start's how-half;
// auto.Provider becomes a Transport selector.)
type Transport interface {
	Launch(ctx context.Context, place Place, spec LaunchSpec) (Attachment, error) // ←Start (how-half)
	Open(ctx context.Context, place Place, name string) (Attachment, bool, error) // net-new (reconnect)
	Attach(ctx context.Context, place Place, name string) error                   // ←Attach
	Name() string                                                                 // transport identity: "tmux", "t3", "acp", "detached"
	Capabilities() TransportCapabilities
}

// Attachment is the live driving surface: the first five verbs ARE the [Carrier]
// verbatim; Observe folds the liveness reads; Close is Stop's how-half.
type Attachment interface {
	Peek(ctx context.Context, lines int) (string, error)                         // ←Carrier.Peek
	Nudge(ctx context.Context, content []ContentBlock) error                     // ←Carrier.Nudge
	SendKeys(ctx context.Context, keys ...string) error                          // ←Carrier.SendKeys
	Interrupt(ctx context.Context) error                                         // ←Carrier.Interrupt
	ClearScrollback(ctx context.Context) error                                   // ←Carrier.ClearScrollback
	Observe(ctx context.Context, processNames []string) (LiveObservation, error) // ←ProcessAlive(names)+IsAttached+GetLastActivity
	Close(ctx context.Context) error                                             // ←Stop (how-half)
}
