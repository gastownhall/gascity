package runtime

import (
	"context"
	"io"
	"time"
)

// This file is the de-conflation facade (PR1): the typed seams the design
// (worker-runtime-transport-v0.md) splits today's monolithic Provider into —
// WHERE (Runtime + Place), HOW (Transport + Attachment), and the WorkerSpec atom
// that ties the five axes together. It is interface-only: the existing
// [Provider] interface and all ~105 call sites are untouched; providers grow
// these implementations one at a time in later phases, each gated on zero
// CoreFingerprint drift. The §-references below cite the design's migration map.
//
// What the rpp-slim carrier work already pre-paid (REUSED, not net-new):
//   - [ExecProvider].Exec      → Place.Exec
//   - [Carrier] (5 verbs)      → Attachment.{Peek,Nudge,SendKeys,Interrupt,ClearScrollback}
//   - [ProviderCapabilities].{CanStream,CanAttachTTY,CanReportActivity,CanReportAttachment}
//     → PlaceCapabilities / TransportCapabilities
//   - proc.exec / proc.stream / tty.attach handshake tokens → the gating below

// WorkerSpec is the de-conflated worker definition: the five independent axes
// plus the run payload (§4). It is the shared atom the whole stack resolves to —
// a controller plan step resolves to a WorkerSpec (§12).
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
// (§11): Stream/AttachTTY are WHERE-properties of the box (a Place can carry a
// stream / a PTY) and ReportActivity says get-last-activity is meaningful. It is
// reported by [Runtime.Capabilities].
type PlaceCapabilities struct {
	ReportActivity bool // get-last-activity is meaningful
	Stream         bool // the box supports proc.stream (a Dial-able Place)
	AttachTTY      bool // the box supports tty.attach (an AttachTTY-able Place)
}

// TransportCapabilities is the HOW half of today's [ProviderCapabilities] (§11):
// ReportAttachment says is-attached is meaningful. Reported by
// [Transport.Capabilities].
type TransportCapabilities struct {
	ReportAttachment bool // is-attached is meaningful
}

// LiveObservation folds the three liveness reads — ProcessAlive, IsAttached,
// GetLastActivity — into one Attachment.Observe (§11).
type LiveObservation struct {
	ProcessAlive bool
	Attached     bool
	LastActivity time.Time
}

// NudgeDelivery controls how Attachment.Nudge delivers (e.g. no-wait vs
// wait-for-idle), folding today's ImmediateNudgeProvider / IdleWaitProvider.
type NudgeDelivery struct {
	NoWait bool
}

// DialRequest is the input to [StreamPlace.Dial]: the argv of the in-box process
// to open a persistent stream to.
type DialRequest struct{ Argv []string }

// AttachRequest is the input to [TTYPlace.AttachTTY]: the in-box target to attach
// an interactive PTY to.
type AttachRequest struct{ Target string }

// Stream is a persistent bidirectional byte channel to an in-box process (acp /
// proc.stream, ssh -T). Net-new (no implementation exists yet).
type Stream interface{ io.ReadWriteCloser }

// TTY is an interactive pseudo-terminal attachment (tty.attach, ssh -t). Net-new
// (no implementation exists yet).
type TTY interface{ io.ReadWriteCloser }

// TranscriptRef points at a session's delivered transcript; the target of
// Attachment.History. Net-new.
type TranscriptRef struct {
	URI string
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
	Env() map[string]string                                        // net-new accessor (env.identity)
}

// StreamPlace is an OPTIONAL Place extension (§6/§13) for boxes that can open a
// persistent bidirectional stream, gated by PlaceCapabilities.Stream (and the
// proc.stream handshake token). A tmux-only box does not implement it; acp does.
type StreamPlace interface {
	Dial(ctx context.Context, req DialRequest) (Stream, error)
}

// TTYPlace is an OPTIONAL Place extension (§6/§13) for boxes that can attach an
// interactive PTY, gated by PlaceCapabilities.AttachTTY (and the tty.attach
// handshake token). A tmux-only box does not implement it.
type TTYPlace interface {
	AttachTTY(ctx context.Context, req AttachRequest) (TTY, error)
}

// Transport is the HOW axis: it launches the agent over a Place and yields an
// Attachment. The tmux Transport's body is the [Carrier] over Place.Exec; acp is
// a stream Transport (NeedsStream). (§11: Start's how-half; auto.Provider becomes
// a Transport selector.)
type Transport interface {
	Launch(ctx context.Context, place Place, spec LaunchSpec) (Attachment, error) // ←Start (how-half)
	Open(ctx context.Context, place Place, name string) (Attachment, bool, error) // net-new (reconnect)
	Attach(ctx context.Context, place Place, name string) error                   // ←Attach (needs TTYPlace when remote)
	Name() string
	NeedsStream() bool // tmux:false, acp:true — gated vs PlaceCapabilities.Stream
	Capabilities() TransportCapabilities
}

// Attachment is the live driving surface: the first five verbs ARE the [Carrier]
// verbatim; Observe folds the liveness reads; History is the transcript seam;
// Close is Stop's how-half.
type Attachment interface {
	Peek(ctx context.Context, lines int) (string, error)                      // ←Carrier.Peek
	Nudge(ctx context.Context, content []ContentBlock, d NudgeDelivery) error // ←Carrier.Nudge
	SendKeys(ctx context.Context, keys ...string) error                       // ←Carrier.SendKeys
	Interrupt(ctx context.Context) error                                      // ←Carrier.Interrupt
	ClearScrollback(ctx context.Context) error                                // ←Carrier.ClearScrollback
	Observe(ctx context.Context) (LiveObservation, error)                     // ←ProcessAlive+IsAttached+GetLastActivity
	History(ctx context.Context) (TranscriptRef, error)                       // net-new (transcript history)
	Close(ctx context.Context) error                                          // ←Stop (how-half)
}
