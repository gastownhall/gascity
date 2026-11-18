package exec

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
)

const programStatusOutputCap = 64 << 10

var (
	programStatusCacheAge      = 45 * time.Second
	programStatusReadTimeout   = 2 * time.Second
	programStatusIdleReadBound = time.Second
	programStatusNow           = time.Now
)

// ProgramStatusAnswer is a pack's root report, without gc's derived counters.
// UnknownState preserves the distinction between an unknown word and no root.
type ProgramStatusAnswer struct {
	State                     runtime.ProgramState
	UnknownState              bool
	Kind                      runtime.ProgramBlockKind
	App                       string
	Progress                  int
	Children, WorkingChildren int
	Exited                    bool
	Incarnation               string
}

type programStatusWire struct {
	State           *string              `json:"state"`
	Kind            *string              `json:"kind"`
	App             *string              `json:"app"`
	Progress        *programStatusNumber `json:"progress"`
	Children        *programStatusNumber `json:"children"`
	WorkingChildren *programStatusNumber `json:"working_children"`
	Exited          *bool                `json:"exited"`
	Incarnation     json.RawMessage      `json:"incarnation"`
}

// programStatusNumber keeps decimal precision until the integer/range check.
// In particular, a fraction must not underflow to zero or round to an integer.
type programStatusNumber string

// UnmarshalJSON preserves numeric tokens passed by the JSON decoder.
func (n *programStatusNumber) UnmarshalJSON(out []byte) error {
	out = bytes.TrimSpace(out)
	if len(out) == 0 || out[0] != '-' && (out[0] < '0' || out[0] > '9') {
		return errors.New("expected a number")
	}
	*n = programStatusNumber(out)
	return nil
}

func programStatusInteger(n *programStatusNumber, limit int64) (int, bool) {
	if n == nil {
		return 0, true
	}
	text := string(*n)
	mantissa, exponent, hasExponent := strings.Cut(strings.ToLower(text), "e")
	if strings.Trim(mantissa, "-0.") == "" {
		return 0, true
	}
	if hasExponent {
		e, err := strconv.ParseInt(exponent, 10, 64)
		// An exponent beyond the mantissa's size cannot yield a small
		// non-zero integer. Bound it before Rat allocates its powers of ten.
		bound := int64(len(text) + 32)
		if err != nil || e > bound || e < -bound {
			return 0, false
		}
	}
	r, ok := new(big.Rat).SetString(text)
	if !ok || !r.IsInt() || r.Sign() < 0 || !r.Num().IsInt64() {
		return 0, false
	}
	v := r.Num().Int64()
	return int(v), v <= limit
}

// ParseProgramStatus accepts exactly one JSON object. Errors never include
// answer bytes. A pack forwards its terminal's root state word unchanged.
func ParseProgramStatus(out []byte) (ProgramStatusAnswer, error) {
	bad := func() (ProgramStatusAnswer, error) {
		return ProgramStatusAnswer{}, fmt.Errorf("malformed program-status answer (%d bytes)", len(out))
	}
	if len(out) > programStatusOutputCap {
		return bad()
	}
	var w programStatusWire
	dec := json.NewDecoder(bytes.NewReader(out))
	if err := dec.Decode(&w); err != nil || w.State == nil {
		return bad()
	}
	if _, err := dec.Token(); err != io.EOF {
		return bad()
	}
	a := ProgramStatusAnswer{State: runtime.ProgramState(*w.State), Progress: -1}
	switch a.State {
	case "", runtime.ProgramIdle, runtime.ProgramWorking, runtime.ProgramDone, runtime.ProgramBlocked, runtime.ProgramError:
	default:
		a.State, a.UnknownState = "", true
	}
	if w.Kind != nil && a.State == runtime.ProgramBlocked {
		switch runtime.ProgramBlockKind(*w.Kind) {
		case runtime.ProgramBlockPermission, runtime.ProgramBlockQuestion, runtime.ProgramBlockAuth:
			a.Kind = runtime.ProgramBlockKind(*w.Kind)
		}
	}
	if w.App != nil && programstatus.ValidApp(*w.App) {
		a.App = *w.App
	}
	if w.Progress != nil && (a.State == runtime.ProgramWorking || a.State == runtime.ProgramBlocked) {
		if v, ok := programStatusInteger(w.Progress, 100); ok {
			a.Progress = v
		}
	}
	var ok bool
	if a.Children, ok = programStatusInteger(w.Children, 1<<31-1); !ok {
		return bad()
	}
	if a.WorkingChildren, ok = programStatusInteger(w.WorkingChildren, 1<<31-1); !ok {
		return bad()
	}
	if a.WorkingChildren > a.Children {
		return bad()
	}
	if w.Exited != nil {
		a.Exited = *w.Exited
	}
	// A present incarnation must be a string; null means none.
	if raw := bytes.TrimSpace(w.Incarnation); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		if err := json.Unmarshal(raw, &a.Incarnation); err != nil {
			return bad()
		}
	}
	if len(a.Incarnation) > 128 {
		return bad()
	}
	for _, b := range []byte(a.Incarnation) {
		if b < 0x21 || b > 0x7e {
			return bad()
		}
	}
	return a, nil
}

// programStatusOp holds a slot until the op ends. It calls begin once the
// slot is held; a canceled read releases the slot without running the op.
func (p *Provider) programStatusOp(ctx context.Context, name string, bound time.Duration, begin func(context.Context) bool) (ProgramStatusAnswer, error) {
	select {
	case p.programs.slots <- struct{}{}:
		defer func() { <-p.programs.slots }()
	case <-ctx.Done():
		return ProgramStatusAnswer{}, fmt.Errorf("program-status %q: %w", name, runtime.ErrRuntimeUnavailable)
	}
	if !begin(ctx) {
		return ProgramStatusAnswer{}, fmt.Errorf("program-status %q: %w", name, runtime.ErrRuntimeUnavailable)
	}
	out, err := p.runCapped(ctx, bound, programStatusOutputCap, nil, "program-status", name)
	if err != nil {
		var exit *opExitError
		if errors.As(err, &exit) && exit.code == runtime.ExitCodeNoScreen {
			return ProgramStatusAnswer{}, fmt.Errorf("program-status %q: %w", name, runtime.ErrSessionNotFound)
		}
		// Do not echo an adapter's stderr or stdout in a status-read error.
		return ProgramStatusAnswer{}, fmt.Errorf("program-status %q failed: %w", name, runtime.ErrRuntimeUnavailable)
	}
	a, err := ParseProgramStatus([]byte(out))
	if err != nil {
		return a, fmt.Errorf("program-status %q: %w: %w", name, runtime.ErrRuntimeUnavailable, err)
	}
	return a, nil
}

type programStatusState struct {
	mu                sync.Mutex
	mode              runtime.ProgramStatusMode
	epoch, generation uint64
	instance          string
	sessions          map[string]*programSession
	slots             chan struct{}
}

type programSession struct {
	gen, ticket, applied uint64
	record               runtime.ProgramStatus
	incarnation          string
	seen, exited         bool
	at                   time.Time
	outcome              runtime.ProgramStatus
	err                  error
	flight               *programFlight
	lastPoll             time.Time
}

type programFlight struct {
	done   chan struct{}
	status runtime.ProgramStatus
	err    error
}

func normalizeProgramStatusMode(mode runtime.ProgramStatusMode) runtime.ProgramStatusMode {
	if mode != runtime.ProgramStatusOn && mode != runtime.ProgramStatusShadow {
		return runtime.ProgramStatusOff
	}
	return mode
}

// WithProgramStatusMode sets the city switch at construction. Unknown modes
// are off; a pack's binding does not gate this pack-owned reading.
func WithProgramStatusMode(mode runtime.ProgramStatusMode) Option {
	return func(p *Provider) { p.SetProgramStatusMode(mode) }
}

// SetProgramStatusMode publishes the live city switch and invalidates reads
// taken under its previous value. An off interval also drops derived history.
func (p *Provider) SetProgramStatusMode(mode runtime.ProgramStatusMode) {
	s := &p.programs
	s.mu.Lock()
	defer s.mu.Unlock()
	mode = normalizeProgramStatusMode(mode)
	if normalizeProgramStatusMode(s.mode) == mode {
		return
	}
	s.mode, s.epoch = mode, s.epoch+1
	for _, e := range s.sessions {
		e.at, e.flight = time.Time{}, nil
		if mode == runtime.ProgramStatusOff {
			s.generation++
			e.gen = s.generation
			e.record = runtime.ProgramStatus{}
			e.seen = false
		}
	}
}

func (p *Provider) programStatusMode() runtime.ProgramStatusMode {
	p.programs.mu.Lock()
	defer p.programs.mu.Unlock()
	return normalizeProgramStatusMode(p.programs.mode)
}

func (p *Provider) programSessionLocked(name string) *programSession {
	s := &p.programs
	if s.sessions == nil {
		s.sessions = make(map[string]*programSession)
	}
	if e := s.sessions[name]; e != nil {
		return e
	}
	s.generation++
	e := &programSession{gen: s.generation}
	s.sessions[name] = e
	return e
}

// forgetProgramStatus drops the entry. Lifecycle ops call it before the op
// and again when the op returns, so overlapping answers cannot update the
// state that exists after the op.
func (p *Provider) forgetProgramStatus(name string) {
	p.programs.mu.Lock()
	defer p.programs.mu.Unlock()
	delete(p.programs.sessions, name)
}

// ProgramStatus shares cold reads and caches for 45s the outcome of every
// read that ran the op, failures included. A read that finds no free slot
// within its bound fails for its callers and caches nothing. Off reads no handshake.
func (p *Provider) ProgramStatus(name string) (runtime.ProgramStatus, error) {
	mode := p.programStatusMode()
	if mode == runtime.ProgramStatusOff {
		return runtime.ProgramStatus{Mode: mode}, nil
	}
	if !p.handshakeCapability(runtime.ProtocolCapabilityProgramStatus) {
		return runtime.ProgramStatus{Mode: mode}, nil
	}
	s := &p.programs
	s.mu.Lock()
	e := p.programSessionLocked(name)
	if age := programStatusNow().Sub(e.at); !e.at.IsZero() && age >= 0 && age < programStatusCacheAge {
		out, err := e.outcome, e.err
		out.Mode = normalizeProgramStatusMode(s.mode)
		s.mu.Unlock()
		return out, err
	}
	f := e.flight
	if f == nil {
		f = &programFlight{done: make(chan struct{})}
		e.flight = f
		go func() {
			f.status, f.err = p.readProgramStatus(context.Background(), name, programStatusReadTimeout)
			s.mu.Lock()
			if e.flight == f {
				e.flight = nil
			}
			s.mu.Unlock()
			close(f.done)
		}()
	}
	s.mu.Unlock()
	<-f.done
	return f.status, f.err
}

// readProgramStatus reads fresh. A read takes its ticket once it holds a
// slot, before its op runs: tickets order admission to execution, not when
// the subprocess starts or the pack samples its state. Completion checks off
// first, then abandonment or replacement, then a missing slot, then whether a
// read with a higher ticket has already applied; only then it applies. Such
// an overtaken read serves that cached outcome, while off returns Unread. A
// read's own apply discards no higher-ticket read, so an answer that predates
// that apply's reset still applies and can reset history again.
func (p *Provider) readProgramStatus(ctx context.Context, name string, bound time.Duration) (runtime.ProgramStatus, error) {
	s := &p.programs
	s.mu.Lock()
	mode := normalizeProgramStatusMode(s.mode)
	if mode == runtime.ProgramStatusOff {
		s.mu.Unlock()
		return runtime.ProgramStatus{Mode: mode}, nil
	}
	e := p.programSessionLocked(name)
	epoch := s.epoch
	// bounded may return before its goroutine. This local is guarded by
	// s.mu, including the context check that prevents a late ticket.
	var ticket uint64
	s.mu.Unlock()
	begin := func(ctx context.Context) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		if ctx.Err() != nil {
			return false
		}
		e.ticket++
		ticket = e.ticket
		return true
	}
	a, expired, err := bounded(ctx, bound, func(ctx context.Context) (ProgramStatusAnswer, error) {
		return p.programStatusOp(ctx, name, bound, begin)
	})
	if expired {
		err = fmt.Errorf("program-status %q timed out: %w", name, runtime.ErrRuntimeUnavailable)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := normalizeProgramStatusMode(s.mode)
	if now == runtime.ProgramStatusOff {
		return runtime.ProgramStatus{Mode: now}, nil
	}
	if ctx.Err() != nil || s.sessions[name] != e || s.epoch != epoch {
		return runtime.ProgramStatus{Mode: now}, fmt.Errorf("program-status %q discarded: %w", name, runtime.ErrRuntimeUnavailable)
	}
	if ticket == 0 {
		return runtime.ProgramStatus{Mode: now}, fmt.Errorf("program-status %q: no read slot within %s: %w", name, bound, runtime.ErrRuntimeUnavailable)
	}
	if ticket <= e.applied {
		out := e.outcome
		out.Mode = now
		return out, e.err
	}
	e.applied = ticket
	out := runtime.ProgramStatus{Mode: mode}
	if err == nil {
		out = p.applyProgramStatusLocked(e, name, a, mode)
	}
	e.at, e.outcome, e.err = programStatusNow(), out, err
	return out, err
}

func (p *Provider) applyProgramStatusLocked(e *programSession, name string, a ProgramStatusAnswer, mode runtime.ProgramStatusMode) runtime.ProgramStatus {
	s := &p.programs
	if e.seen && (e.incarnation != a.Incarnation || e.exited && !a.Exited) {
		s.generation++
		e.gen, e.record = s.generation, runtime.ProgramStatus{}
	}
	e.seen, e.incarnation, e.exited = true, a.Incarnation, a.Exited
	nonce := ""
	if a.Incarnation != "" {
		sum := sha256.Sum256([]byte(name + "\x00" + a.Incarnation))
		nonce = fmt.Sprintf("%s.%d.%s", s.instance, e.gen, hex.EncodeToString(sum[:16]))
	}
	r := &e.record
	if a.Exited {
		return runtime.ProgramStatus{Mode: mode, Validity: runtime.ProgramStatusExited, Nonce: nonce, LastError: r.LastError}
	}
	if a.UnknownState {
		return runtime.ProgramStatus{Mode: mode, Validity: runtime.ProgramStatusSilent, Nonce: nonce}
	}
	before := *r
	prev, next := r.State, a.State
	if next != "" {
		r.Reported = true
	}
	if next == runtime.ProgramError {
		r.LastError = true
	}
	boundary := func(st runtime.ProgramState) bool {
		return st == runtime.ProgramIdle || st == runtime.ProgramDone || st == runtime.ProgramError
	}
	busy := func(st runtime.ProgramState) bool {
		return st == runtime.ProgramWorking || st == runtime.ProgramBlocked
	}
	if next != prev {
		r.Since = programStatusNow()
		if next == "" {
			r.Since = time.Time{}
		}
		if busy(next) && (prev == "" || boundary(prev)) {
			r.TurnStarts++
		}
		if busy(prev) && boundary(next) {
			r.TurnEnds++
			r.QuietSince = programStatusNow()
		}
	}
	r.State, r.Kind, r.App, r.Progress = next, a.Kind, a.App, a.Progress
	r.Children, r.WorkingChildren = a.Children, a.WorkingChildren
	if *r != before {
		r.Seq++
	}
	if next == "" {
		validity := runtime.ProgramStatusSilent
		if r.Reported {
			validity = runtime.ProgramStatusCleared
		}
		return runtime.ProgramStatus{Mode: mode, Validity: validity, Nonce: nonce, Reported: r.Reported, LastError: r.LastError}
	}
	out := *r
	out.Mode, out.Validity, out.Nonce = mode, runtime.ProgramStatusValid, nonce
	return out
}

func newProgramStatusState() programStatusState {
	var id [16]byte
	_, _ = rand.Read(id[:]) // crypto/rand.Read fills the buffer or terminates the process.
	return programStatusState{instance: hex.EncodeToString(id[:]), slots: make(chan struct{}, 8)}
}
