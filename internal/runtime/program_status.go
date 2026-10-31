package runtime

import (
	"strings"
	"time"
)

// ProgramState is the root word of an OSC 7501 report (Program Status
// Protocol spec rev 0.4).
type ProgramState string

// The OSC 7501 root states.
const (
	ProgramIdle    ProgramState = "idle"
	ProgramWorking ProgramState = "working"
	ProgramDone    ProgramState = "done"
	ProgramBlocked ProgramState = "blocked"
	ProgramError   ProgramState = "error"
)

// ProgramBlockKind qualifies ProgramBlocked. Unknown kinds read as unspecified.
type ProgramBlockKind string

// The OSC 7501 block kinds.
const (
	ProgramBlockUnspecified ProgramBlockKind = ""
	ProgramBlockPermission  ProgramBlockKind = "permission"
	ProgramBlockQuestion    ProgramBlockKind = "question"
	ProgramBlockAuth        ProgramBlockKind = "auth"
)

// ProgramStatusMode is the city switch a reading was taken under.
type ProgramStatusMode string

// The city switch's values.
const (
	ProgramStatusOff    ProgramStatusMode = "off"
	ProgramStatusShadow ProgramStatusMode = "shadow" // advisory: observe and compare, never decide
	ProgramStatusOn     ProgramStatusMode = "on"     // a Valid reading may change a decision
)

// ProgramStatusValidity says whether a reading may be used and, if not, why.
// The zero value is Unread, so a zero ProgramStatus carries no state.
type ProgramStatusValidity string

// The validities a reading is judged to.
const (
	ProgramStatusUnread     ProgramStatusValidity = ""            // switch off, or capability gate failed
	ProgramStatusValid      ProgramStatusValidity = "valid"       // a root record under our nonce, its reader attached
	ProgramStatusNotArmed   ProgramStatusValidity = "not_armed"   // no nonce of ours bound to this pane (incl. inherited values)
	ProgramStatusSilent     ProgramStatusValidity = "silent"      // armed; the root has not reported this incarnation
	ProgramStatusCleared    ProgramStatusValidity = "cleared"     // the root reported, then was removed (state=clear or OSC 133;A)
	ProgramStatusReaderLost ProgramStatusValidity = "reader_lost" // pipe closed, or no live tap with our nonce
	ProgramStatusExited     ProgramStatusValidity = "exited"      // pane dead, or the caller's liveness says the agent is gone
)

// ProgramStatus is the agent program's own OSC 7501 root record, judged
// against the live pane in the same read. It is the program's claim:
// positive-only evidence with no heartbeat. Counters compare only within
// one Nonce (one capture incarnation). Text (title, msg) is never carried here.
type ProgramStatus struct {
	Mode            ProgramStatusMode // stamped on every reading; "" reads as off
	Validity        ProgramStatusValidity
	State           ProgramState     // "" unless Validity == Valid
	Kind            ProgramBlockKind // only with ProgramBlocked
	App             string
	Progress        int       // -1 when absent
	Since           time.Time // root entered State (capture host clock)
	QuietSince      time.Time // root last left working/blocked for a boundary; zero if never
	Nonce           string    // "<rand>.<pane_id>"
	Seq             uint64
	TurnStarts      uint32 // entries into working or blocked from a boundary or no root; working↔blocked is not a start
	TurnEnds        uint32 // exits from working/blocked to a boundary
	Children        int
	WorkingChildren int
	Probed          bool // the program sent the support probe this incarnation (doctor)
	LastError       bool // an error report was seen; sticky across clear (annotation only)
	Reported        bool // a root report was seen this incarnation: no root then reads cleared, not silent
	ReaderVerified  bool // the process table confirmed the tap; false when the snapshot was unavailable
}

// Valid reports whether the reading may be used: judged valid, with a root
// state.
func (s ProgramStatus) Valid() bool { return s.Validity == ProgramStatusValid && s.State != "" }

// Decides reports whether the reading may change a decision: valid and taken
// under the switch's on mode. A shadow reading is advisory.
func (s ProgramStatus) Decides() bool { return s.Valid() && s.Mode == ProgramStatusOn }

// Running reports a valid working root.
func (s ProgramStatus) Running() bool { return s.Valid() && s.State == ProgramWorking }

// Blocked reports a valid blocked root: neither a boundary nor running.
func (s ProgramStatus) Blocked() bool { return s.Valid() && s.State == ProgramBlocked }

// InputBoundary reports a valid root at an input boundary (idle, done or
// error). It reads the root only, never its children.
func (s ProgramStatus) InputBoundary() bool { return s.Valid() && s.State.boundary() }

// Quiescent reports an input boundary with no working child.
func (s ProgramStatus) Quiescent() bool { return s.InputBoundary() && s.WorkingChildren == 0 }

// known reports whether st is a root word of spec rev 0.4.
func (st ProgramState) known() bool {
	return st.boundary() || st == ProgramWorking || st == ProgramBlocked
}

// boundary reports whether st is an input boundary: idle, done or error.
func (st ProgramState) boundary() bool {
	return st == ProgramIdle || st == ProgramDone || st == ProgramError
}

// ProgramStatusHoldLimit bounds every hold derived from a record (the
// idle-claim nudge's busy-hold limit).
const ProgramStatusHoldLimit = 10 * time.Minute

// HoldLive reports whether a Running/Blocked/WorkingChildren hold may still
// count: the reading is valid and 0 <= now - max(Since, lastActivity) <
// ProgramStatusHoldLimit. Past the bound a caller must act as if no record
// existed. It never reads idle. A Since or lastActivity later than now (a
// forged or skewed record, a clock stepped back) is no evidence and holds
// nothing, so no hold outlasts the bound.
func (s ProgramStatus) HoldLive(lastActivity, now time.Time) bool {
	if !s.Valid() {
		return false
	}
	from := s.Since
	if lastActivity.After(from) {
		from = lastActivity
	}
	if from.IsZero() || from.After(now) {
		return false
	}
	return now.Sub(from) < ProgramStatusHoldLimit
}

// ProgramBaseline is taken before gc types; AdvancedPast is positive delivery
// evidence against it.
type ProgramBaseline struct {
	Nonce      string
	Seq        uint64
	TurnStarts uint32
	State      ProgramState // "" unless taken from a valid reading
}

// Baseline is the reading's position for a later AdvancedPast. A reading that
// is not valid records no state, so it credits nothing.
func (s ProgramStatus) Baseline() ProgramBaseline {
	b := ProgramBaseline{Nonce: s.Nonce, Seq: s.Seq, TurnStarts: s.TurnStarts}
	if s.Valid() {
		b.State = s.State
	}
	return b
}

// AdvancedPast reports positive evidence that the program moved past b: the
// reading is valid under b's nonce, b's state is a boundary (idle, done or
// error), and a turn started since (TurnStarts > b.TurnStarts) or the record
// changed into working or blocked (Seq > b.Seq). A busy, silent or
// other-nonce baseline credits nothing.
func (s ProgramStatus) AdvancedPast(b ProgramBaseline) bool {
	if !s.Valid() || b.Nonce == "" || s.Nonce != b.Nonce || !b.State.boundary() {
		return false
	}
	busy := s.State == ProgramWorking || s.State == ProgramBlocked
	return s.TurnStarts > b.TurnStarts || (s.Seq > b.Seq && busy)
}

// ProgramStatusProvider is an optional extension: the leaf's judged reading of
// the session's agent pane. Composites do not implement it; it is read on the
// routed leaf ([ProgramStatusFor]), as [TurnRunningProvider] is. With the
// switch off it returns an Unread reading and nil error; a pane it has not
// armed reads not_armed; a missing session returns an error wrapping
// [ErrSessionNotFound]; an unobservable runtime returns one wrapping
// [ErrRuntimeUnavailable].
type ProgramStatusProvider interface {
	ProgramStatus(name string) (ProgramStatus, error)
}

// ProgramStatusFor returns the [ProgramStatusProvider] of the backend serving
// session name, the leaf [ResolveBackend] names, and false when that backend
// takes no such reading.
func ProgramStatusFor(p Provider, name string) (ProgramStatusProvider, bool) {
	leaf, _, _ := ResolveBackend(p, name)
	ps, ok := leaf.(ProgramStatusProvider)
	return ps, ok
}

// ProgramStatusArm is a session's capture declaration, resolved from provider
// data, the city switch and the transport. The zero value declares nothing.
type ProgramStatusArm struct {
	Capture bool   // arm the pane-output reader
	App     string // reader drops reports naming another program
	Relay   bool   // wrap the agent in the probe-answering relay (only with Capture)
}

// ProgramCaptureAction is what one heal call did.
type ProgramCaptureAction string

// The heal outcomes.
const (
	ProgramCaptureUnchanged ProgramCaptureAction = "unchanged"
	ProgramCaptureArmed     ProgramCaptureAction = "armed"
	ProgramCaptureDisarmed  ProgramCaptureAction = "disarmed"
	ProgramCaptureDeferred  ProgramCaptureAction = "deferred" // pipe busy, pane dead, or backoff
)

// ProgramCaptureHealer is an optional extension the controller calls on the
// runtime inventory lane to converge a session's capture to want. Composites
// do not implement it ([ProgramCaptureHealerFor] resolves the leaf).
type ProgramCaptureHealer interface {
	HealProgramCapture(name string, want ProgramStatusArm) (ProgramCaptureAction, error)
}

// ProgramCaptureHealerFor returns the [ProgramCaptureHealer] of the backend
// serving session name, the leaf [ResolveBackend] names, and false when that
// backend does not heal capture.
func ProgramCaptureHealerFor(p Provider, name string) (ProgramCaptureHealer, bool) {
	leaf, _, _ := ResolveBackend(p, name)
	h, ok := leaf.(ProgramCaptureHealer)
	return h, ok
}

// ProgramStatusComparison is one shadow-mode decision: today's reading beside
// what the record would decide. Closed tokens only: no field carries program
// text (title, msg) or prompt text.
type ProgramStatusComparison struct {
	Session   string             // the session's runtime name
	SessionID string             // the session's bead id, when the point knows it
	Family    string             // provider family (resolved built-in ancestor); config data, not a role
	Point     ProgramStatusPoint // the decision point
	Scrape    ProgramVerdict     // today's reading, which decided
	Program   ProgramVerdict     // what the record-side rule would decide
	Status    ProgramStatus      // the reading the record-side rule judged
	LatencyMs int64              // program signal minus scrape signal; meaningful only when Timed
	Timed     bool
	Dropped   int // comparisons the emitter dropped, under pressure, since its last one
	// HoldActivity is the runtime's last activity the rule judged hold
	// liveness with ([ProgramStatus.HoldLive]); zero when it used none.
	HoldActivity time.Time
	// JudgedAt is when the rule judged; zero means when the comparison is
	// recorded. With HoldActivity it makes the recorded hold liveness the
	// rule's own.
	JudgedAt time.Time
}

// HoldLive is the hold liveness the comparison's rule judged with: the
// reading's [ProgramStatus.HoldLive] under HoldActivity at JudgedAt, or at
// recordedAt when JudgedAt is zero.
func (c ProgramStatusComparison) HoldLive(recordedAt time.Time) bool {
	at := c.JudgedAt
	if at.IsZero() {
		at = recordedAt
	}
	return c.Status.HoldLive(c.HoldActivity, at)
}

// ProgramStatusComparisonSink receives shadow comparisons. Implementations
// must not block the caller.
type ProgramStatusComparisonSink func(ProgramStatusComparison)

// ProgramTapLiveness is the process table's word on whether a pane's capture
// reader (the tap holding the pane's nonce) is alive. The zero value is
// unknown: the snapshot was unavailable.
type ProgramTapLiveness string

// The tap-liveness tristate.
const (
	ProgramTapUnknown ProgramTapLiveness = ""
	ProgramTapAlive   ProgramTapLiveness = "yes"
	ProgramTapGone    ProgramTapLiveness = "no"
)

// ProgramRecordForm is how the published record value decoded. The zero value
// is absent: nothing was published.
type ProgramRecordForm string

// The record forms a codec reports.
const (
	ProgramRecordAbsent         ProgramRecordForm = ""
	ProgramRecordDecoded        ProgramRecordForm = "decoded"
	ProgramRecordUndecodable    ProgramRecordForm = "undecodable"
	ProgramRecordUnknownVersion ProgramRecordForm = "unknown_version" // a codec version this gc does not know
)

// ProgramPaneRow is one agent-pane read for [JudgeProgramStatus]: the pane's
// own fields, the tap nonce bound to it, the decoded record, the tap's
// liveness from the process table, the mode and capability gate, and the
// caller's liveness fact. The zero value judges Unread.
type ProgramPaneRow struct {
	Mode        ProgramStatusMode
	CaptureGate bool   // the capture capability gate passed
	PaneID      string // the row's pane id ("%3")
	PaneDead    bool
	PanePipe    bool               // a pipe-pane reader is attached
	Tap         string             // the pane's tap nonce option as read ("<rand>.<pane_id>")
	TapLiveness ProgramTapLiveness // a process holding Tap's nonce, from the process table
	AgentGone   bool               // the caller's liveness says the agent is gone
	RecordForm  ProgramRecordForm
	Record      ProgramStatus // the decoded record; read only when RecordForm is decoded
}

// JudgeProgramStatus derives a reading from one agent-pane row. The first
// matching rule wins:
//
//  1. mode off (or unknown), or the capture gate failed: Unread;
//  2. Tap empty, or not "<rand>.<PaneID>" for this row's pane: not_armed;
//  3. pane dead, or the agent gone: exited (LastError kept under our nonce);
//  4. pipe closed, or the tap gone: reader_lost;
//  5. a record of an unknown codec version under a live tap: reader_lost;
//     otherwise an absent or undecodable record, one whose nonce is not Tap
//     exactly, or one with an unknown root word: silent (treated as absent);
//  6. no root: silent if no root was ever reported this incarnation (a
//     probe or child reports alone leave it silent), else cleared;
//  7. valid, ReaderVerified when the process table confirmed the tap.
//
// Only a valid reading carries state. The others carry none of the record's
// claims, only its annotations (LastError under rule 3; Probed, Reported and
// LastError under rule 6).
func JudgeProgramStatus(row ProgramPaneRow) ProgramStatus {
	mode := row.Mode
	if mode != ProgramStatusShadow && mode != ProgramStatusOn {
		return ProgramStatus{Mode: ProgramStatusOff}
	}
	if !row.CaptureGate {
		return ProgramStatus{Mode: mode}
	}
	rand, armed := strings.CutSuffix(row.Tap, "."+row.PaneID)
	if row.PaneID == "" || !armed || rand == "" {
		return ProgramStatus{Mode: mode, Validity: ProgramStatusNotArmed}
	}
	ours := row.RecordForm == ProgramRecordDecoded && row.Record.Nonce == row.Tap &&
		(row.Record.State == "" || row.Record.State.known())
	if row.PaneDead || row.AgentGone {
		return ProgramStatus{Mode: mode, Validity: ProgramStatusExited, Nonce: row.Tap, LastError: ours && row.Record.LastError}
	}
	if !row.PanePipe || row.TapLiveness == ProgramTapGone ||
		(row.RecordForm == ProgramRecordUnknownVersion && row.TapLiveness == ProgramTapAlive) {
		return ProgramStatus{Mode: mode, Validity: ProgramStatusReaderLost, Nonce: row.Tap}
	}
	if !ours {
		return ProgramStatus{Mode: mode, Validity: ProgramStatusSilent, Nonce: row.Tap}
	}
	if row.Record.State == "" {
		return judgeNoRoot(mode, row.Record)
	}
	s := row.Record
	s.Mode, s.Validity = mode, ProgramStatusValid
	s.ReaderVerified = row.TapLiveness == ProgramTapAlive
	s.Kind = blockKind(s.State, s.Kind)
	return s
}

// judgeNoRoot judges a record of ours with no root (rule 6). It keeps the
// incarnation's annotations and none of the record's claims: Probed is what
// doctor reads to tell a probe that went unanswered from a program that never
// probed, and Reported is what tells cleared from silent.
func judgeNoRoot(mode ProgramStatusMode, rec ProgramStatus) ProgramStatus {
	s := ProgramStatus{
		Mode:      mode,
		Validity:  ProgramStatusSilent,
		Nonce:     rec.Nonce,
		Probed:    rec.Probed,
		LastError: rec.LastError,
		Reported:  rec.Reported,
	}
	if rec.Reported {
		s.Validity = ProgramStatusCleared
	}
	return s
}

// blockKind is the kind a judged root carries: k on a blocked root when spec
// rev 0.4 names it, otherwise unspecified.
func blockKind(st ProgramState, k ProgramBlockKind) ProgramBlockKind {
	if st == ProgramBlocked && (k == ProgramBlockPermission || k == ProgramBlockQuestion || k == ProgramBlockAuth) {
		return k
	}
	return ProgramBlockUnspecified
}
