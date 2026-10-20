package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// The environment keys that name a session incarnation. Session starts
// publish all three (session.RuntimeEnv).
const (
	// RuntimeEpochEnv carries the incarnation's generation.
	RuntimeEpochEnv = "GC_RUNTIME_EPOCH"
	// ContinuationEpochEnv carries the incarnation's continuation epoch.
	ContinuationEpochEnv = "GC_CONTINUATION_EPOCH"
	// InstanceTokenEnv carries the incarnation's instance token.
	InstanceTokenEnv = "GC_INSTANCE_TOKEN"
)

// inputFenceDigestTag versions the digest format. Live panes hold digests in
// this format, so a new format needs a new tag and a new pane option name: a
// pane never re-adopts a fence it already holds.
const inputFenceDigestTag = "gascity/fence/v1"

// InputFence names the session incarnation an input effect must reach: the
// generation (GC_RUNTIME_EPOCH), continuation epoch (GC_CONTINUATION_EPOCH)
// and instance token (GC_INSTANCE_TOKEN) it was started with. Token is a
// capability (see envsecret.go): every formatting verb, GoString and
// MarshalJSON show only Digest, and runtimes store Digest, never Token.
type InputFence struct {
	Generation int64
	Epoch      int64
	Token      string
}

// IsZero reports whether f names nothing at all: the "no fence" value.
func (f InputFence) IsZero() bool { return f == InputFence{} }

// Validate reports a fence that names no incarnation: an empty token, or a
// generation or epoch below 1. The error wraps ErrInvalidInputFence.
func (f InputFence) Validate() error {
	switch {
	case f.Token == "":
		return fmt.Errorf("%w: no instance token", ErrInvalidInputFence)
	case f.Generation < 1:
		return fmt.Errorf("%w: generation %d", ErrInvalidInputFence, f.Generation)
	case f.Epoch < 1:
		return fmt.Errorf("%w: continuation epoch %d", ErrInvalidInputFence, f.Epoch)
	}
	return nil
}

// Digest is the fence's public identity: the first 16 hex characters of
// sha256 over the length-prefixed tag, generation, epoch and token (decimal
// counters, uvarint length prefixes). It is safe for argv, options, logs and
// JSON, and equal exactly when the fences are.
func (f InputFence) Digest() string {
	buf := fenceLP(nil, inputFenceDigestTag)
	buf = fenceLP(buf, strconv.FormatInt(f.Generation, 10))
	buf = fenceLP(buf, strconv.FormatInt(f.Epoch, 10))
	buf = fenceLP(buf, f.Token)
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])[:16]
}

// fenceLP appends s with a uvarint length prefix (the opjournal convention).
func fenceLP(buf []byte, s string) []byte {
	buf = binary.AppendUvarint(buf, uint64(len(s)))
	return append(buf, s...)
}

// String names the fence by its counters and digest.
func (f InputFence) String() string {
	if f.IsZero() {
		return "fence(none)"
	}
	return fmt.Sprintf("fence(gen=%d epoch=%d digest=%s)", f.Generation, f.Epoch, f.Digest())
}

// GoString is String's Go-syntax form; it shows the digest, not the token.
func (f InputFence) GoString() string {
	return fmt.Sprintf("runtime.InputFence{Generation:%d, Epoch:%d, Digest:%q}", f.Generation, f.Epoch, f.Digest())
}

// Format prints String (GoString for %#v) under every verb, so no verb can
// reach the token through the struct's fields.
func (f InputFence) Format(s fmt.State, verb rune) {
	if verb == 'v' && s.Flag('#') {
		_, _ = fmt.Fprint(s, f.GoString())
		return
	}
	_, _ = fmt.Fprint(s, f.String())
}

// MarshalJSON encodes the generation, epoch and digest, never the token.
func (f InputFence) MarshalJSON() ([]byte, error) {
	digest := ""
	if !f.IsZero() {
		digest = f.Digest()
	}
	return json.Marshal(struct {
		Generation int64  `json:"generation"`
		Epoch      int64  `json:"epoch"`
		Digest     string `json:"digest"`
	}{f.Generation, f.Epoch, digest})
}

// InputFenceFromEnv reads the incarnation an environment names: the token
// verbatim (required: false without one), and the generation and epoch via
// strconv.Atoi, where a missing, malformed or non-positive value reads as 1,
// as the session layer's starts default them.
func InputFenceFromEnv(env map[string]string) (InputFence, bool) {
	token := env[InstanceTokenEnv]
	if token == "" {
		return InputFence{}, false
	}
	return InputFence{
		Generation: positiveOrOne(env[RuntimeEpochEnv]),
		Epoch:      positiveOrOne(env[ContinuationEpochEnv]),
		Token:      token,
	}, true
}

func positiveOrOne(raw string) int64 {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 1
	}
	return int64(n)
}

// InputFencing is how a runtime applies an input fence to a session.
type InputFencing string

const (
	// InputFencingAtomic means the check and the input are one runtime step:
	// no other incarnation can receive a fenced input, except on an input
	// path the runtime documents as checked and takes by runtime state,
	// which the static trait cannot report (tmux: a detached Gemini
	// session's input through its hidden attach client).
	InputFencingAtomic InputFencing = "atomic"
	// InputFencingChecked means the runtime checks, then inputs: an
	// incarnation started in between can receive the input.
	InputFencingChecked InputFencing = "checked"
	// InputFencingNone means the runtime cannot fence the session's input:
	// fenced requests are refused with ErrInputFenceUnsupported.
	InputFencingNone InputFencing = "none"
)

var (
	// ErrFenceMismatch reports that the runtime found another incarnation, or
	// none, where a fenced input was to go. Runtimes return a
	// *FenceMismatchError.
	ErrFenceMismatch = errors.New("runtime incarnation does not match the input fence")
	// ErrInputFenceUnsupported reports a fenced request to a runtime that
	// cannot fence the session's input. Nothing was sent.
	ErrInputFenceUnsupported = errors.New("runtime cannot fence this session's input")
	// ErrInvalidInputFence reports a fence that names no incarnation.
	ErrInvalidInputFence = errors.New("input fence names no incarnation")
)

// What a FenceMismatchError found instead of a fence digest.
const (
	// FenceFoundAbsent: no session (or agent pane) by that name.
	FenceFoundAbsent = "absent"
	// FenceFoundDead: the session's agent process has exited.
	FenceFoundDead = "dead"
	// FenceFoundUnproven: the runtime cannot tell which incarnation runs.
	FenceFoundUnproven = "unproven"
)

// FenceMismatchError is ErrFenceMismatch with what the runtime found. Found
// is the digest it found, or FenceFoundAbsent, FenceFoundDead or
// FenceFoundUnproven. Partial reports that some of the operation's effect
// reached the fenced incarnation before the miss: a nudge's text, or any key
// of a keys, interrupt or ctx-fenced operation. Input that only prepares a
// nudge (a copy-mode exit, a composer clear, a dialog key before its text)
// does not count: a nudge that missed before its text is not partial.
type FenceMismatchError struct {
	Session string
	Found   string
	Partial bool
}

// Error names the session and what was found; never a token.
func (e *FenceMismatchError) Error() string {
	partial := ""
	if e.Partial {
		partial = ", after partial input"
	}
	return fmt.Sprintf("session %q: %s (found %s%s)", e.Session, ErrFenceMismatch.Error(), e.Found, partial)
}

// Is matches ErrFenceMismatch only.
func (e *FenceMismatchError) Is(target error) bool { return target == ErrFenceMismatch }

// FencedNudge is one fenced nudge. Immediate skips the runtime's own idle
// wait (as NudgeNow does); DeliveryKey is used by a runtime that
// deduplicates deliveries. Receipt, when not nil, is set by a runtime that
// deduplicates keyed deliveries ([KeyedNudgeProvider]) to its receipt once
// the nudge succeeded; other runtimes leave it as is.
type FencedNudge struct {
	Fence       InputFence
	Immediate   bool
	DeliveryKey string
	Receipt     *DeliveryReceipt
}

// FencedInputProvider is implemented by runtimes that can make input to a
// session conditional on its incarnation. When InputFencing(name) is not
// none, the runtime's ctx-taking input methods (InputClearProvider.ClearInput,
// DialogProvider.DismissKnownDialogs and
// InterruptedTurnResetProvider.ResetInterruptedTurn) apply the fence
// InputFenceFrom(ctx) names, as these methods do; a runtime (or a
// composite's routed backend) that cannot fence refuses them under a fence
// (RefuseUnfenceableCtx). A fenced call never reports success into an
// absent, dead or other incarnation: it returns a *FenceMismatchError,
// ErrInvalidInputFence, or (InputFencing none) ErrInputFenceUnsupported
// before any input. A runtime with no server to ask at all (tmux with no
// server running) returns ErrRuntimeUnavailable instead of a mismatch: it
// cannot tell absent from unreachable.
type FencedInputProvider interface {
	// InputFencing reports how the runtime fences session name's input. It
	// is a static trait: it reads no runtime state.
	InputFencing(name string) InputFencing
	// NudgeFenced delivers content as Nudge (or NudgeNow when n.Immediate)
	// does, only to the incarnation n.Fence names.
	NudgeFenced(ctx context.Context, name string, content []ContentBlock, n FencedNudge) error
	// SendKeysFenced sends keys as SendKeys does, only to fence's incarnation.
	SendKeysFenced(ctx context.Context, name string, fence InputFence, keys ...string) error
	// InterruptFenced interrupts as Interrupt does, only fence's incarnation.
	InterruptFenced(ctx context.Context, name string, fence InputFence) error
}

// InputFencingFor reports how p fences session name's input: none when p
// does not implement FencedInputProvider or reports a value this build does
// not know.
func InputFencingFor(p Provider, name string) InputFencing {
	fp, ok := p.(FencedInputProvider)
	if !ok {
		return InputFencingNone
	}
	switch v := fp.InputFencing(name); v {
	case InputFencingAtomic, InputFencingChecked:
		return v
	default:
		return InputFencingNone
	}
}

// FencingBackend returns backend's fenced input surface for session name,
// for a composite provider forwarding FencedInputProvider to the backend it
// routes name to. A backend that cannot fence name is
// ErrInputFenceUnsupported: a composite never falls through to another
// backend.
func FencingBackend(backend Provider, name string) (FencedInputProvider, error) {
	fp, ok := backend.(FencedInputProvider)
	if !ok || InputFencingFor(backend, name) == InputFencingNone {
		return nil, fmt.Errorf("session %q: %w", name, ErrInputFenceUnsupported)
	}
	return fp, nil
}

// RefuseUnfenceableCtx returns ErrInputFenceUnsupported when ctx carries an
// input fence and p cannot fence session name, so a ctx-fenced ClearInput,
// DismissKnownDialogs or ResetInterruptedTurn is never run unfenced. A
// composite calls it before forwarding to the routed backend.
func RefuseUnfenceableCtx(ctx context.Context, p Provider, name string) error {
	if _, ok := InputFenceFrom(ctx); ok && InputFencingFor(p, name) == InputFencingNone {
		return fmt.Errorf("session %q: %w", name, ErrInputFenceUnsupported)
	}
	return nil
}

type inputFenceContextKey struct{}

// WithInputFence returns ctx carrying f: input sent under it must reach f's
// incarnation. The latest fence wins; a zero fence clears it.
func WithInputFence(ctx context.Context, f InputFence) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, inputFenceContextKey{}, f)
}

// InputFenceFrom returns the fence ctx carries, false when it carries none.
func InputFenceFrom(ctx context.Context) (InputFence, bool) {
	if ctx == nil {
		return InputFence{}, false
	}
	f, _ := ctx.Value(inputFenceContextKey{}).(InputFence)
	return f, !f.IsZero()
}

// LivenessFenceMiss is what an input fenced by ctx reports when a fresh
// liveness read of session name (live, err) rules its incarnation out before
// anything is sent: a read that cannot answer is ErrRuntimeUnavailable, never
// a miss; no session is a miss finding it absent; an exited agent or a
// corpse is one finding it dead. It is nil when ctx carries no fence (the
// input keeps its own handling) or the read finds the agent alive.
func LivenessFenceMiss(ctx context.Context, name string, live Liveness, err error) error {
	if _, fenced := InputFenceFrom(ctx); !fenced {
		return nil
	}
	switch {
	case err != nil && errors.Is(err, ErrRuntimeUnavailable):
		return fmt.Errorf("session %q: %w", name, err)
	case err != nil:
		return fmt.Errorf("session %q: %w: %w", name, ErrRuntimeUnavailable, err)
	case live.Running && live.Alive:
		return nil
	case live.Running || live.Corpse:
		return &FenceMismatchError{Session: name, Found: FenceFoundDead}
	}
	return &FenceMismatchError{Session: name, Found: FenceFoundAbsent}
}

// fencedProvider is p's fenced input surface for session name under the
// fence ctx carries: the fence (validated), p as a FencedInputProvider, and
// whether ctx carries a fence at all. A provider that cannot fence name is
// ErrInputFenceUnsupported.
func fencedProvider(ctx context.Context, p Provider, name string) (InputFence, FencedInputProvider, bool, error) {
	f, ok := InputFenceFrom(ctx)
	if !ok {
		return InputFence{}, nil, false, nil
	}
	if err := f.Validate(); err != nil {
		return f, nil, true, err
	}
	fp, err := FencingBackend(p, name)
	return f, fp, true, err
}

// NudgeWithContext delivers content to name through p. When ctx carries no
// fence it is the legacy dispatch: NudgeNow when immediate and p is an
// ImmediateNudgeProvider (unless ctx's delivery key goes to a leaf that
// deduplicates keyed deliveries), else NudgeWithDeliveryKey. With a fence it
// is p's NudgeFenced carrying ctx's delivery key, or ErrInputFenceUnsupported
// when p cannot fence name. It is [NudgeWithReceipt] without the receipt.
func NudgeWithContext(ctx context.Context, p Provider, name string, content []ContentBlock, immediate bool) error {
	_, err := NudgeWithReceipt(ctx, p, name, content, immediate)
	return err
}

// SendKeysWithContext sends keys to name through p: SendKeysFenced under the
// fence ctx carries, else p.SendKeys.
func SendKeysWithContext(ctx context.Context, p Provider, name string, keys ...string) error {
	f, fp, fenced, err := fencedProvider(ctx, p, name)
	if err != nil {
		return err
	}
	if fenced {
		return fp.SendKeysFenced(ctx, name, f, keys...)
	}
	return p.SendKeys(name, keys...)
}

// InterruptWithContext interrupts name through p: InterruptFenced under the
// fence ctx carries, else p.Interrupt.
func InterruptWithContext(ctx context.Context, p Provider, name string) error {
	f, fp, fenced, err := fencedProvider(ctx, p, name)
	if err != nil {
		return err
	}
	if fenced {
		return fp.InterruptFenced(ctx, name, f)
	}
	return p.Interrupt(name)
}
