package opjournal

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// Row shape constants.
const (
	// BeadType is the bead type of op and fact rows. It is ready-excluded.
	BeadType = "op"
	// LabelOp marks an op row.
	LabelOp = "gc:op"
	// LabelFact marks a fact row.
	LabelFact = "gc:fact"
	// SchemaRev is the op and fact schema revision this build writes and
	// reads.
	SchemaRev = 1

	labelOpConvPrefix   = "gc:op-conv:"
	labelFactConvPrefix = "gc:fact-conv:"
	beadStatusOpen      = "open"
	beadStatusClosed    = "closed"
)

// ConversationLabel is the per-conversation label of op rows. It stays on the
// row for history reads; ListOpen reads by gc.op.open instead.
func ConversationLabel(conv string) string { return labelOpConvPrefix + conv }

// FactConversationLabel is the per-conversation key label of fact rows.
func FactConversationLabel(conv string) string { return labelFactConvPrefix + conv }

// Op metadata keys.
const (
	keySchema        = "gc.op.schema"
	keyKind          = "gc.op.kind"
	keyExecutor      = "gc.op.executor"
	keyOpen          = "gc.op.open"
	keyConversation  = "gc.op.conversation"
	keyPrincipal     = "gc.op.principal"
	keyActor         = "gc.op.actor"
	keyOwnerEpoch    = "gc.op.owner_epoch"
	keyClientKey     = "gc.op.client_key"
	keyClientSeq     = "gc.op.client_seq"
	keyPayload       = "gc.op.payload"
	keyPayloadDigest = "gc.op.payload_digest"

	keyWhen          = "gc.op.when"
	keyWake          = "gc.op.wake"
	keyRole          = "gc.op.role"
	keyOnUnsupported = "gc.op.on_unsupported"
	keyNowGeneration = "gc.op.now_gen"
	keyNowTurn       = "gc.op.now_turn"
	keyOnFailure     = "gc.op.on_failure"
	keyRestartMode   = "gc.op.restart_mode"
	keyKillGrace     = "gc.op.kill_grace"
	keyGuarantee     = "gc.op.guarantee"

	keyScope            = "gc.op.scope"
	keyScopeEpoch       = "gc.op.scope_epoch"
	keyScopeGeneration  = "gc.op.scope_generation"
	keyScopeTurn        = "gc.op.scope_turn"
	keyScopeInteraction = "gc.op.scope_interaction"

	keyAfter       = "gc.op.after"
	keyNotBefore   = "gc.op.not_before"
	keyStartBy     = "gc.op.start_by"
	keyFinishBy    = "gc.op.finish_by"
	keyCausation   = "gc.op.causation"
	keyCorrelation = "gc.op.correlation"
	keyCreatedAt   = "gc.op.created_at"

	keyStatus      = "gc.op.status"
	keyTransitions = "gc.op.transitions"
	keyLaneSeq     = "gc.op.lane_seq"
	keyAttempt     = "gc.op.attempt"

	keyCancelBy     = "gc.op.cancel_by"
	keyCancelAt     = "gc.op.cancel_at"
	keyCancelReason = "gc.op.cancel_reason"

	keyOutcome             = "gc.op.outcome"
	keyOutcomeReason       = "gc.op.outcome_reason"
	keyOutcomeDegradation  = "gc.op.outcome_degradation"
	keyResultGeneration    = "gc.op.result_generation"
	keyResultTurn          = "gc.op.result_turn"
	keyResultEpoch         = "gc.op.result_epoch"
	keyResultVia           = "gc.op.result_via"
	keyResultCoalescedInto = "gc.op.result_coalesced_into"
)

// requestFields returns the digest-covered metadata of op: kind, policy,
// scope, after and payload. Empty values mean "absent". The executor is not
// covered.
func requestFields(op Op) map[string]string {
	p, s := op.Policy, op.Scope
	return map[string]string{
		keyKind:             string(op.Kind),
		keyWhen:             string(p.Message.When),
		keyWake:             string(p.Message.Wake),
		keyRole:             string(p.Message.Role),
		keyOnUnsupported:    string(p.Message.OnUnsupported),
		keyNowGeneration:    formatInt(p.Message.NowGeneration),
		keyNowTurn:          p.Message.NowTurn,
		keyOnFailure:        string(p.Interrupt.OnFailure),
		keyRestartMode:      string(p.Restart.Mode),
		keyKillGrace:        formatDuration(p.Kill.Grace),
		keyGuarantee:        string(p.Guarantee),
		keyScope:            string(s.Kind),
		keyScopeEpoch:       formatInt(s.Epoch),
		keyScopeGeneration:  formatInt(s.Generation),
		keyScopeTurn:        s.Turn,
		keyScopeInteraction: s.Interaction,
		keyAfter:            op.After,
		keyPayload:          op.Payload,
	}
}

type transitionJSON struct {
	Status Status `json:"status"`
	At     string `json:"at"`
	By     string `json:"by"`
}

// EncodeOp renders op as its bead row. It refuses an op that would not decode
// back: an unknown kind, an id that does not match its key, or any field
// outside the schema. It writes gc.op.open, the conversation id, while the op
// is not closed, so ListOpen reads open rows by one metadata key; Revision is
// never encoded.
func EncodeOp(op Op) (beads.Bead, error) {
	spec, err := Lookup(op.Kind)
	if err != nil {
		return beads.Bead{}, err
	}
	if op.SchemaRev != SchemaRev {
		return beads.Bead{}, fmt.Errorf("%w: encoding revision %d", ErrUnsupportedSchema, op.SchemaRev)
	}
	if err := checkOpID(op); err != nil {
		return beads.Bead{}, err
	}
	if !validDigest(op.PayloadDigest) {
		return beads.Bead{}, invalidf("payload digest %q is not a sha256 hex digest", op.PayloadDigest)
	}
	if err := validateRequestShape(op, spec); err != nil {
		return beads.Bead{}, err
	}
	if err := validateState(op, spec); err != nil {
		return beads.Bead{}, err
	}
	transitions, err := encodeTransitions(op.Transitions)
	if err != nil {
		return beads.Bead{}, err
	}
	meta := requestFields(op)
	open := op.Conversation
	if op.Status == StatusClosed {
		open = ""
	}
	fields := map[string]string{
		keySchema:        strconv.Itoa(SchemaRev),
		keyExecutor:      string(op.Executor),
		keyOpen:          open,
		keyConversation:  op.Conversation,
		keyPrincipal:     op.Producer.Principal,
		keyActor:         op.Producer.Actor,
		keyOwnerEpoch:    formatInt(op.Producer.OwnerEpoch),
		keyClientKey:     op.ClientKey,
		keyClientSeq:     formatUint(op.ClientSeq),
		keyPayloadDigest: op.PayloadDigest,
		keyNotBefore:     formatOptionalTime(op.NotBefore),
		keyStartBy:       formatOptionalTime(op.StartBy),
		keyFinishBy:      formatOptionalTime(op.FinishBy),
		keyCausation:     op.Causation,
		keyCorrelation:   op.Correlation,
		keyCreatedAt:     formatOptionalTime(op.CreatedAt),
		keyStatus:        string(op.Status),
		keyTransitions:   transitions,
		keyLaneSeq:       formatUint(op.LaneSeq),
		keyAttempt:       formatInt(int64(op.Attempt)),
	}
	for k, v := range fields {
		meta[k] = v
	}
	for k, v := range cancelFields(op.Cancel) {
		meta[k] = v
	}
	for k, v := range outcomeFields(op.Outcome) {
		meta[k] = v
	}
	for k, v := range meta {
		if v == "" {
			delete(meta, k)
		}
	}
	status := beadStatusOpen
	if op.Status == StatusClosed {
		status = beadStatusClosed
	}
	return beads.Bead{
		ID:        op.ID,
		Title:     "op " + string(op.Kind) + " " + op.Conversation,
		Status:    status,
		Type:      BeadType,
		Assignee:  op.Lease.String(),
		Labels:    []string{LabelOp, ConversationLabel(op.Conversation)},
		Metadata:  meta,
		NoHistory: true,
	}, nil
}

func cancelFields(c Cancel) map[string]string {
	return map[string]string{
		keyCancelBy:     c.By,
		keyCancelAt:     formatOptionalTime(c.At),
		keyCancelReason: c.Reason,
	}
}

func outcomeFields(o Outcome) map[string]string {
	return map[string]string{
		keyOutcome:             string(o.Arm),
		keyOutcomeReason:       o.Reason,
		keyOutcomeDegradation:  o.Degradation,
		keyResultGeneration:    formatInt(o.Result.Generation),
		keyResultTurn:          o.Result.Turn,
		keyResultEpoch:         formatInt(o.Result.ContextEpoch),
		keyResultVia:           o.Result.Via,
		keyResultCoalescedInto: o.Result.CoalescedInto,
	}
}

// checkOpID verifies op.ID is <prefix>-op-<hash of its key fields>.
func checkOpID(op Op) error {
	suffix := opIDInfix + opIDSuffix(op.Conversation, op.Producer.Principal, op.ClientKey)
	prefix, ok := strings.CutSuffix(op.ID, suffix)
	if !ok {
		return corruptf(op.ID, "id does not match its conversation, principal and client key")
	}
	if err := validatePrefix(prefix); err != nil {
		return corruptf(op.ID, "id prefix: %v", err)
	}
	return nil
}

// DecodeOp reads an op row. A row without gc.op.executor reads
// ExecutorLegacy, and Revision is the bead's. It fails loudly — ErrUnsupportedKind for a kind
// this build does not know, ErrUnsupportedSchema for a newer schema,
// ErrUnsupportedValue for a vocabulary value it does not know, and
// ErrCorruptOp for anything malformed — and never skips a row silently.
//
// Two projections reconcile the bead status with gc.op.status, which is
// authoritative: an op closed in metadata whose bead is still open (a torn
// close) reads closed, and a bead closed outside the journal whose metadata is
// still open reads closed with outcome failed{closed_externally}. Decoding
// never writes; the store's next Close, Cancel or Transition of the row stores
// that outcome.
func DecodeOp(b beads.Bead) (Op, error) {
	if b.Type != BeadType || !slices.Contains(b.Labels, LabelOp) {
		return Op{}, corruptf(b.ID, "not an op row (type %q, labels %v)", b.Type, b.Labels)
	}
	d := metaDecoder{id: b.ID, meta: b.Metadata}
	if err := d.schema(keySchema); err != nil {
		return Op{}, err
	}
	kind := Kind(d.required(keyKind))
	if d.err != nil {
		return Op{}, d.err
	}
	spec, err := Lookup(kind)
	if err != nil {
		return Op{}, fmt.Errorf("%w: row %s has kind %q", ErrUnsupportedKind, b.ID, kind)
	}
	executor := ExecutorKind(d.meta[keyExecutor])
	if executor == "" {
		executor = ExecutorLegacy
	}
	op := Op{
		ID:            b.ID,
		Conversation:  d.required(keyConversation),
		Kind:          kind,
		Executor:      executor,
		Revision:      b.Revision,
		SchemaRev:     SchemaRev,
		ClientKey:     d.required(keyClientKey),
		Payload:       d.meta[keyPayload],
		PayloadDigest: d.required(keyPayloadDigest),
		Policy: Policy{
			Guarantee: Guarantee(d.meta[keyGuarantee]),
			Message: MessagePolicy{
				When: When(d.meta[keyWhen]), Wake: Wake(d.meta[keyWake]),
				Role: Role(d.meta[keyRole]), OnUnsupported: Fallback(d.meta[keyOnUnsupported]),
				NowGeneration: d.int64(keyNowGeneration), NowTurn: d.meta[keyNowTurn],
			},
			Interrupt: InterruptPolicy{OnFailure: OnFailure(d.meta[keyOnFailure])},
			Restart:   RestartPolicy{Mode: RestartMode(d.meta[keyRestartMode])},
			Kill:      KillPolicy{Grace: d.duration(keyKillGrace)},
		},
		Scope: Scope{
			Kind:        ScopeKind(d.meta[keyScope]),
			Epoch:       d.int64(keyScopeEpoch),
			Generation:  d.int64(keyScopeGeneration),
			Turn:        d.meta[keyScopeTurn],
			Interaction: d.meta[keyScopeInteraction],
		},
		After:     d.meta[keyAfter],
		ClientSeq: d.uint64(keyClientSeq),
		NotBefore: d.time(keyNotBefore),
		StartBy:   d.time(keyStartBy),
		FinishBy:  d.time(keyFinishBy),
		Producer: Producer{
			Principal:  d.required(keyPrincipal),
			Actor:      d.meta[keyActor],
			OwnerEpoch: d.int64(keyOwnerEpoch),
		},
		Causation:   d.meta[keyCausation],
		Correlation: d.meta[keyCorrelation],
		CreatedAt:   d.time(keyCreatedAt),
		Status:      Status(d.required(keyStatus)),
		LaneSeq:     d.uint64(keyLaneSeq),
		Attempt:     d.int(keyAttempt),
		Cancel: Cancel{
			By:     d.meta[keyCancelBy],
			At:     d.time(keyCancelAt),
			Reason: d.meta[keyCancelReason],
		},
		Outcome: Outcome{
			Arm:         Arm(d.meta[keyOutcome]),
			Reason:      d.meta[keyOutcomeReason],
			Degradation: d.meta[keyOutcomeDegradation],
			Result: Result{
				Generation:    d.int64(keyResultGeneration),
				Turn:          d.meta[keyResultTurn],
				ContextEpoch:  d.int64(keyResultEpoch),
				Via:           d.meta[keyResultVia],
				CoalescedInto: d.meta[keyResultCoalescedInto],
			},
		},
		Transitions: d.transitions(keyTransitions),
	}
	if d.err != nil {
		return Op{}, d.err
	}
	if b.Assignee != "" {
		lease, err := ParseLease(b.Assignee)
		if err != nil {
			return Op{}, corruptf(b.ID, "assignee is not a lane lease: %v", err)
		}
		op.Lease = lease
	}
	if err := checkVocabulary(b.ID, op); err != nil {
		return Op{}, err
	}
	if !slices.Contains(b.Labels, ConversationLabel(op.Conversation)) {
		return Op{}, corruptf(b.ID, "missing label %s", ConversationLabel(op.Conversation))
	}
	if open := d.meta[keyOpen]; open != "" && open != op.Conversation {
		return Op{}, corruptf(b.ID, "%s %q is not its conversation %s", keyOpen, open, op.Conversation)
	}
	if err := checkOpID(op); err != nil {
		return Op{}, err
	}
	if !validDigest(op.PayloadDigest) {
		return Op{}, corruptf(b.ID, "payload digest %q is not a sha256 hex digest", op.PayloadDigest)
	}
	if err := validateRequestShape(op, spec); err != nil {
		return Op{}, corruptf(b.ID, "%v", err)
	}
	if err := validateState(op, spec); err != nil {
		return Op{}, corruptf(b.ID, "%v", err)
	}
	if closedExternally(b) {
		op.Status = StatusClosed
		op.Outcome = Outcome{Arm: ArmFailed, Reason: ReasonClosedExternally}
	}
	return op, nil
}

// closedExternally reports whether op row b's bead was closed outside the
// journal: the bead is closed and gc.op.status is not.
func closedExternally(b beads.Bead) bool {
	return b.Status == beadStatusClosed && b.Metadata[keyStatus] != string(StatusClosed)
}

// checkVocabulary reports a policy or scope value outside this build's
// vocabulary as ErrUnsupportedValue. Whether a known value suits the op's
// kind is validateRequestShape's question, and a misfit is corruption.
func checkVocabulary(id string, op Op) error {
	p := op.Policy
	for _, f := range []struct {
		key, value string
		known      bool
	}{
		{keyWhen, string(p.Message.When), slices.Contains(whens, p.Message.When)},
		{keyWake, string(p.Message.Wake), slices.Contains(wakes, p.Message.Wake)},
		{keyRole, string(p.Message.Role), slices.Contains(roles, p.Message.Role)},
		{keyOnUnsupported, string(p.Message.OnUnsupported), slices.Contains(fallbacks, p.Message.OnUnsupported)},
		{keyOnFailure, string(p.Interrupt.OnFailure), slices.Contains(onFailures, p.Interrupt.OnFailure)},
		{keyRestartMode, string(p.Restart.Mode), slices.Contains(restartModes, p.Restart.Mode)},
		{keyGuarantee, string(p.Guarantee), slices.Contains(guarantees, p.Guarantee)},
		{keyScope, string(op.Scope.Kind), slices.Contains(scopeKinds, op.Scope.Kind)},
		{keyExecutor, string(op.Executor), op.Executor.Valid()},
	} {
		if f.value != "" && !f.known {
			return fmt.Errorf("%w: row %s has %s %q", ErrUnsupportedValue, id, f.key, f.value)
		}
	}
	return nil
}

func encodeTransitions(ts []Transition) (string, error) {
	if len(ts) == 0 {
		return "", nil
	}
	out := make([]transitionJSON, len(ts))
	for i, t := range ts {
		out[i] = transitionJSON{Status: t.Status, At: formatTime(t.At), By: t.By}
	}
	data, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("encoding op transitions: %w", err)
	}
	return string(data), nil
}

// metaDecoder reads typed metadata values, keeping the first error.
type metaDecoder struct {
	id   string
	meta map[string]string
	err  error
}

func (d *metaDecoder) fail(key, format string, args ...any) {
	if d.err == nil {
		d.err = corruptf(d.id, "%s: %s", key, fmt.Sprintf(format, args...))
	}
}

func (d *metaDecoder) schema(key string) error {
	v := d.meta[key]
	if v == "" {
		return corruptf(d.id, "%s is missing", key)
	}
	rev, err := strconv.Atoi(v)
	if err != nil {
		return corruptf(d.id, "%s %q is not a number", key, v)
	}
	if rev != SchemaRev {
		return fmt.Errorf("%w: row %s has revision %d, this build reads %d", ErrUnsupportedSchema, d.id, rev, SchemaRev)
	}
	return nil
}

func (d *metaDecoder) required(key string) string {
	v := d.meta[key]
	if v == "" {
		d.fail(key, "missing")
	}
	return v
}

func (d *metaDecoder) int64(key string) int64 {
	v := d.meta[key]
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || formatInt(n) != v {
		d.fail(key, "%q is not a canonical integer", v)
	}
	return n
}

// int reads an integer that must fit this platform's int: a wider value is
// refused, never wrapped.
func (d *metaDecoder) int(key string) int {
	v := d.meta[key]
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || formatInt(int64(n)) != v {
		d.fail(key, "%q is not a canonical integer within int range", v)
	}
	return n
}

func (d *metaDecoder) uint64(key string) uint64 {
	v := d.meta[key]
	if v == "" {
		return 0
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil || formatUint(n) != v {
		d.fail(key, "%q is not a canonical unsigned integer", v)
	}
	return n
}

func (d *metaDecoder) time(key string) time.Time {
	v := d.meta[key]
	if v == "" {
		return time.Time{}
	}
	t, err := parseTime(v)
	if err != nil {
		d.fail(key, "%q: %v", v, err)
	}
	return t
}

func (d *metaDecoder) duration(key string) time.Duration {
	v := d.meta[key]
	if v == "" {
		return 0
	}
	dur, err := time.ParseDuration(v)
	if err != nil {
		d.fail(key, "%q: %v", v, err)
	}
	return dur
}

func (d *metaDecoder) transitions(key string) []Transition {
	v := d.meta[key]
	if v == "" {
		return nil
	}
	var raw []transitionJSON
	if err := json.Unmarshal([]byte(v), &raw); err != nil {
		d.fail(key, "%v", err)
		return nil
	}
	out := make([]Transition, len(raw))
	for i, r := range raw {
		at, err := parseTime(r.At)
		if err != nil {
			d.fail(key, "entry %d time %q: %v", i, r.At, err)
			return nil
		}
		out[i] = Transition{Status: r.Status, At: at, By: r.By}
	}
	return out
}

func formatInt(n int64) string {
	if n == 0 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}

func formatUint(n uint64) string {
	if n == 0 {
		return ""
	}
	return strconv.FormatUint(n, 10)
}

func formatDuration(d time.Duration) string {
	if d == 0 {
		return ""
	}
	return d.String()
}

func formatOptionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return formatTime(t)
}
