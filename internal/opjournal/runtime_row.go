package opjournal

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// Runtime row shape.
const (
	// LabelRuntime marks a conversation's runtime row.
	LabelRuntime = "gc:rt"

	labelRuntimeConvPrefix = "gc:rt-conv:"
	// rtIDTag keeps runtime row ids disjoint from every other hash scheme.
	rtIDTag = "gascity/rt-id/v1"
	// runtimeIDInfix separates the mint prefix from the hash.
	runtimeIDInfix = "-rt-"
)

// RuntimeConversationLabel is the per-conversation key label of the runtime
// row.
func RuntimeConversationLabel(conv string) string { return labelRuntimeConvPrefix + conv }

// RuntimeRowID returns the pinned id of conversation's runtime row in the
// namespace prefix: <prefix>-rt-<hash16("gascity/rt-id/v1", lp(conversation))>.
// prefix is the op store's mint prefix, as for op ids.
func RuntimeRowID(prefix, conversation string) (string, error) {
	if err := validatePrefix(prefix); err != nil {
		return "", err
	}
	if err := validateConversation(conversation); err != nil {
		return "", err
	}
	return prefix + runtimeIDInfix + runtimeIDSuffix(conversation), nil
}

func runtimeIDSuffix(conversation string) string { return hash16(rtIDTag, conversation) }

// Runtime row metadata keys. The opjournal codec owns every gc.rt.* name; no
// session field class covers them.
const (
	keyRTSchema       = "gc.rt.schema"
	keyRTConversation = "gc.rt.conversation"

	keyRTGen   = "gc.rt.gen"
	keyRTEpoch = "gc.rt.epoch"
	keyRTSeq   = "gc.rt.seq"
	keyRTLease = "gc.rt.lease"

	keyRTTurnCaps       = "gc.rt.turn.caps"
	keyRTTurnGen        = "gc.rt.turn.gen"
	keyRTTurnID         = "gc.rt.turn.id"
	keyRTTurnState      = "gc.rt.turn.state"
	keyRTTurnAt         = "gc.rt.turn.at"
	keyRTTurnLastID     = "gc.rt.turn.last_id"
	keyRTTurnLastEnd    = "gc.rt.turn.last_end"
	keyRTTurnLastAt     = "gc.rt.turn.last_at"
	keyRTTurnSeen       = "gc.rt.turn.seen"
	keyRTTurnObservedAt = "gc.rt.turn.observed_at"

	keyRTState      = "gc.rt.state"
	keyRTStateAt    = "gc.rt.state_at"
	keyRTAttached   = "gc.rt.attached"
	keyRTAttachedAt = "gc.rt.attached_at"
	keyRTActivityAt = "gc.rt.activity_at"
	keyRTObserver   = "gc.rt.observer"

	keyRTBindingThread          = "gc.rt.binding.thread"
	keyRTBindingProject         = "gc.rt.binding.project"
	keyRTBindingProviderSession = "gc.rt.binding.provider_session"

	keyRTHost         = "gc.rt.host"
	keyRTHostGen      = "gc.rt.host_gen"
	keyRTHostKinds    = "gc.rt.host_kinds"
	keyRTHostProtocol = "gc.rt.host_protocol"
	keyRTHostBuild    = "gc.rt.host_build"
	keyRTHostSince    = "gc.rt.host_since"
)

var runtimeKeys = []string{
	keyRTSchema, keyRTConversation, keyRTGen, keyRTEpoch, keyRTSeq, keyRTLease,
	keyRTTurnCaps, keyRTTurnGen, keyRTTurnID, keyRTTurnState, keyRTTurnAt,
	keyRTTurnLastID, keyRTTurnLastEnd, keyRTTurnLastAt, keyRTTurnSeen, keyRTTurnObservedAt,
	keyRTState, keyRTStateAt, keyRTAttached, keyRTAttachedAt, keyRTActivityAt, keyRTObserver,
	keyRTBindingThread, keyRTBindingProject, keyRTBindingProviderSession,
	keyRTHost, keyRTHostGen, keyRTHostKinds, keyRTHostProtocol, keyRTHostBuild, keyRTHostSince,
}

// updateTextKeys hold free text a fenced update writes; they are refused when
// bd would type them as a JSON scalar.
var updateTextKeys = []string{
	keyRTTurnID, keyRTTurnLastID, keyRTTurnLastEnd, keyRTTurnSeen, keyRTObserver,
	keyRTBindingThread, keyRTBindingProject, keyRTBindingProviderSession,
	keyRTHost, keyRTHostProtocol, keyRTHostBuild,
}

// RuntimeState is the coarse runtime state (gc.rt.state). This build writes
// only the values below; a row may carry a newer build's value, which decodes
// as read and reads as not observed. Empty means not observed.
type RuntimeState string

// Runtime states.
const (
	RuntimeStarting    RuntimeState = "starting"
	RuntimeReady       RuntimeState = "ready"
	RuntimeUnavailable RuntimeState = "unavailable"
	RuntimeExited      RuntimeState = "exited"
)

var runtimeStates = []RuntimeState{RuntimeStarting, RuntimeReady, RuntimeUnavailable, RuntimeExited}

// Attachment says whether a terminal is attached to the runtime
// (gc.rt.attached). This build writes only the values below; a row may carry
// a newer build's value, which decodes as read and reads as not observed.
// Empty means not observed.
type Attachment string

// Attachments.
const (
	AttachmentAttached Attachment = "attached"
	AttachmentDetached Attachment = "detached"
)

var attachments = []Attachment{AttachmentAttached, AttachmentDetached}

// RuntimeBinding holds the runtime-side ids of a conversation
// (gc.rt.binding.*). A provider session id never identifies a gc session.
type RuntimeBinding struct {
	Thread          string
	Project         string
	ProviderSession string
}

// HostStamp is the stamp a host other than the controller writes when it
// serves the conversation's lane (gc.rt.host*). A reader ignores a stamp
// whose Generation is not the current one.
type HostStamp struct {
	// Host is the serving host, such as box:<box id>#<daemon nonce>.
	Host string
	// Generation is the generation the stamp is for.
	Generation int64
	// Kinds are the conversation-lane kinds the host serves.
	Kinds []Kind
	// Protocol is the host's wire protocol version, a token.
	Protocol string
	// Build is the host's gc build id.
	Build string
	// Since is when the stamp was written.
	Since time.Time
}

// RuntimeRow is one conversation's runtime row: a bead of type "op" with
// labels gc:rt and gc:rt-conv:<conversation>, no history, never ephemeral,
// at the pinned id RuntimeRowID. Lanes create it lazily at their first Take
// and read and update it under revision guards; this codec only maps it.
//
// Age semantics. The lane cells (Seq and Lease, with the Generation and Epoch
// lanes serve) are authoritative: lanes write them under the row's revision
// guard. Every other cell is an observation, published on transitions only
// (no heartbeat), and every time cell beside it (each _at, and the turn
// cells' at and last_at) is the observer's clock: when the observer saw the
// transition, not when it happened. A reader may use an observation only as
// a hint of that age, never as proof that a runtime or turn is live or gone.
// The source's own times live on the facts.
//
// Writers: lanes at Take and Done (Generation, Epoch, Seq, Lease); the turn
// observer only (Turn); the lane host or box daemon on observed transitions
// (State, Attached, ActivityAt, Observer); the runtime binding (Binding); and
// a serving host (Host).
type RuntimeRow struct {
	ID           string
	Conversation string
	// Closed is the bead's status: a pass that closes the conversation closes
	// its runtime row, which stays readable for history.
	Closed bool

	// Generation and Epoch are the incarnation lanes serve (gc.rt.gen,
	// gc.rt.epoch).
	Generation int64
	Epoch      int64
	// Seq is the last lane_seq assigned (gc.rt.seq).
	Seq uint64
	// Lease is the in-flight Take's lease, or zero (gc.rt.lease).
	Lease Lease

	// Turn holds the gc.rt.turn.* cells.
	Turn TurnCells

	// State and StateAt are the coarse runtime state (gc.rt.state,
	// gc.rt.state_at).
	State   RuntimeState
	StateAt time.Time
	// Attached, AttachedAt and ActivityAt are coarse observations
	// (gc.rt.attached, gc.rt.attached_at, gc.rt.activity_at).
	Attached   Attachment
	AttachedAt time.Time
	ActivityAt time.Time
	// Observer names who writes the observation cells, such as ctl:<host>
	// or box:<id> (gc.rt.observer).
	Observer string

	Binding RuntimeBinding
	Host    HostStamp

	// Extra holds every metadata key this build does not own, such as a
	// gc.rt.* key a newer build added. Encoding writes it back unchanged;
	// RuntimeRowPatch never writes it.
	Extra map[string]string
}

// EncodeRuntimeRow renders r as its bead row under prefix. It refuses a row
// that would not decode back.
func EncodeRuntimeRow(prefix string, r RuntimeRow) (beads.Bead, error) {
	id, err := RuntimeRowID(prefix, r.Conversation)
	if err != nil {
		return beads.Bead{}, err
	}
	if r.ID != "" && r.ID != id {
		return beads.Bead{}, invalidf("runtime row id %q does not match its conversation (%s)", r.ID, id)
	}
	if err := r.validate(); err != nil {
		return beads.Bead{}, err
	}
	meta := r.ownedFields()
	for k, v := range meta {
		if v == "" {
			delete(meta, k)
		}
	}
	for k, v := range r.Extra {
		meta[k] = v
	}
	status := beadStatusOpen
	if r.Closed {
		status = beadStatusClosed
	}
	return beads.Bead{
		ID:        id,
		Title:     "runtime " + r.Conversation,
		Status:    status,
		Type:      BeadType,
		Labels:    []string{LabelRuntime, RuntimeConversationLabel(r.Conversation)},
		Metadata:  meta,
		NoHistory: true,
	}, nil
}

// ownedFields returns every owned key with its encoding, "" for absent.
func (r RuntimeRow) ownedFields() map[string]string {
	t, h := r.Turn, r.Host
	return map[string]string{
		keyRTSchema:       strconv.Itoa(SchemaRev),
		keyRTConversation: r.Conversation,
		keyRTGen:          formatInt(r.Generation),
		keyRTEpoch:        formatInt(r.Epoch),
		keyRTSeq:          formatUint(r.Seq),
		keyRTLease:        r.Lease.String(),

		keyRTTurnCaps:       t.Caps.String(),
		keyRTTurnGen:        formatInt(t.Generation),
		keyRTTurnID:         t.ID,
		keyRTTurnState:      string(t.State),
		keyRTTurnAt:         formatOptionalTime(t.At),
		keyRTTurnLastID:     t.LastID,
		keyRTTurnLastEnd:    string(t.LastEnd),
		keyRTTurnLastAt:     formatOptionalTime(t.LastAt),
		keyRTTurnSeen:       t.Seen,
		keyRTTurnObservedAt: formatOptionalTime(t.ObservedAt),

		keyRTState:      string(r.State),
		keyRTStateAt:    formatOptionalTime(r.StateAt),
		keyRTAttached:   string(r.Attached),
		keyRTAttachedAt: formatOptionalTime(r.AttachedAt),
		keyRTActivityAt: formatOptionalTime(r.ActivityAt),
		keyRTObserver:   r.Observer,

		keyRTBindingThread:          r.Binding.Thread,
		keyRTBindingProject:         r.Binding.Project,
		keyRTBindingProviderSession: r.Binding.ProviderSession,

		keyRTHost:         h.Host,
		keyRTHostGen:      formatInt(h.Generation),
		keyRTHostKinds:    joinKinds(h.Kinds),
		keyRTHostProtocol: h.Protocol,
		keyRTHostBuild:    h.Build,
		keyRTHostSince:    formatOptionalTime(h.Since),
	}
}

func (r RuntimeRow) validate() error {
	if err := r.validateShape(); err != nil {
		return err
	}
	return r.validateVocabulary(func(string) bool { return true })
}

// validateShape checks everything but membership of the observation
// vocabularies, which a newer build may extend: a value there need only be a
// token. A decoded row passes it with a newer build's values in place.
func (r RuntimeRow) validateShape() error {
	if err := validateConversation(r.Conversation); err != nil {
		return err
	}
	if r.Generation < 0 || r.Epoch < 0 || r.Turn.Generation < 0 || r.Host.Generation < 0 {
		return invalidf("runtime row generations and epoch must be >= 0")
	}
	if !r.Lease.IsZero() {
		if err := r.Lease.validate(); err != nil {
			return err
		}
		if r.Lease.Conversation != r.Conversation {
			return invalidf("runtime row lease %s is for conversation %s", r.Lease, r.Lease.Conversation)
		}
	}
	if err := r.Turn.validate(); err != nil {
		return err
	}
	if err := errors.Join(
		validateOptionalToken("runtime state", string(r.State)),
		validateOptionalToken("runtime attachment", string(r.Attached)),
	); err != nil {
		return err
	}
	if err := errors.Join(
		validateOptionalText("runtime observer", r.Observer),
		validateOptionalText("binding thread", r.Binding.Thread),
		validateOptionalText("binding project", r.Binding.Project),
		validateOptionalText("binding provider session", r.Binding.ProviderSession),
		validateOptionalText("host", r.Host.Host),
		validateOptionalText("host build", r.Host.Build),
	); err != nil {
		return err
	}
	if r.Host.Protocol != "" {
		if err := validateToken("host protocol", r.Host.Protocol); err != nil {
			return err
		}
	}
	if err := validateHostKinds(r.Host.Kinds); err != nil {
		return err
	}
	for k, v := range r.Extra {
		if slices.Contains(runtimeKeys, k) {
			return invalidf("runtime row extra key %s is owned by the codec", k)
		}
		if k == "" || v == "" {
			return invalidf("runtime row extra key %q has an empty name or value", k)
		}
	}
	return nil
}

// validateVocabulary refuses an observation value this build does not know,
// for the keys check selects.
func (r RuntimeRow) validateVocabulary(check func(key string) bool) error {
	switch {
	case check(keyRTTurnState) && r.Turn.State != "" && !slices.Contains(turnStates, r.Turn.State):
		return invalidf("turn state %q", r.Turn.State)
	case check(keyRTState) && r.State != "" && !slices.Contains(runtimeStates, r.State):
		return invalidf("runtime state %q", r.State)
	case check(keyRTAttached) && r.Attached != "" && !slices.Contains(attachments, r.Attached):
		return invalidf("runtime attachment %q", r.Attached)
	}
	if check(keyRTHostKinds) {
		for _, k := range r.Host.Kinds {
			if _, err := Lookup(k); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateOptionalToken(what, v string) error {
	if v == "" {
		return nil
	}
	return validateToken(what, v)
}

// validate checks the cells' shape. A turn state this build does not know
// need only be a token, and may carry a turn id.
func (c TurnCells) validate() error {
	switch c.State {
	case TurnStateRunning:
		if err := validateText("turn id", c.ID, maxClientKeyLen); err != nil {
			return err
		}
	case "", TurnStateEnded, TurnStateUnknown:
		if c.ID != "" {
			return invalidf("turn id %q set while the turn state is %q", c.ID, c.State)
		}
	default:
		if err := validateToken("turn state", string(c.State)); err != nil {
			return err
		}
		if c.ID != "" {
			if err := validateText("turn id", c.ID, maxClientKeyLen); err != nil {
				return err
			}
		}
	}
	if c.LastID != "" {
		if err := validateText("last turn id", c.LastID, maxClientKeyLen); err != nil {
			return err
		}
	}
	if c.LastEnd != "" {
		if err := validateToken("last turn end", string(c.LastEnd)); err != nil {
			return err
		}
	}
	if c.Seen != "" {
		if err := validateText("seen turn id", c.Seen, maxClientKeyLen); err != nil {
			return err
		}
	}
	return c.Caps.validate()
}

func (cs TurnCapsStamp) validate() error {
	seen := make(map[string]bool, len(cs))
	for _, c := range cs {
		if c.Key == "" || c.Value == "" {
			return invalidf("turn capability %q=%q is empty", c.Key, c.Value)
		}
		if err := errors.Join(validateToken("turn capability key", c.Key), validateToken("turn capability value", c.Value)); err != nil {
			return err
		}
		if seen[c.Key] {
			return invalidf("turn capability %q appears twice", c.Key)
		}
		seen[c.Key] = true
	}
	return nil
}

// String renders the stamp as k=v pairs joined by ",".
func (cs TurnCapsStamp) String() string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = c.Key + "=" + c.Value
	}
	return strings.Join(parts, ",")
}

// ParseTurnCapsStamp parses a stamp written by TurnCapsStamp.String.
func ParseTurnCapsStamp(s string) (TurnCapsStamp, error) {
	if s == "" {
		return nil, nil
	}
	var out TurnCapsStamp
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return nil, invalidf("turn capability %q is not k=v", part)
		}
		out = append(out, TurnCapsPair{Key: k, Value: v})
	}
	if err := out.validate(); err != nil {
		return nil, err
	}
	return out, nil
}

// validateHostKinds checks the kinds' shape: tokens, no repeats, and a kind
// this build knows must be a conversation-lane kind. A kind it does not know
// is left to validateVocabulary.
func validateHostKinds(kinds []Kind) error {
	for i, k := range kinds {
		if err := validateToken("host kind", string(k)); err != nil {
			return err
		}
		if spec, err := Lookup(k); err == nil && spec.Lane != LaneConversation {
			return invalidf("host kind %s is not a conversation-lane kind", k)
		}
		if slices.Contains(kinds[:i], k) {
			return invalidf("host kind %s appears twice", k)
		}
	}
	return nil
}

func joinKinds(kinds []Kind) string {
	parts := make([]string, len(kinds))
	for i, k := range kinds {
		parts[i] = string(k)
	}
	return strings.Join(parts, ",")
}

// DecodeRuntimeRow reads a runtime row. It fails loudly: ErrUnsupportedSchema
// for a newer schema, and ErrCorruptOp for anything malformed or an id that
// is not <prefix>-rt-<hash of its conversation> under a valid prefix. Keys it
// does not own land in Extra. An observation value a newer build added (a
// turn or runtime state, an attachment, a host kind) is a hint, not a
// failure: it is kept as read and reads as unknown, so one newer observer
// never stops this build's lanes from writing the row's lane cells (DES-O1
// §4.1). Only the codec's encode and RuntimeRowPatch refuse to write one.
func DecodeRuntimeRow(b beads.Bead) (RuntimeRow, error) {
	if b.Type != BeadType || !slices.Contains(b.Labels, LabelRuntime) {
		return RuntimeRow{}, corruptf(b.ID, "not a runtime row (type %q, labels %v)", b.Type, b.Labels)
	}
	d := metaDecoder{id: b.ID, meta: b.Metadata}
	if err := d.schema(keyRTSchema); err != nil {
		return RuntimeRow{}, err
	}
	r := RuntimeRow{
		ID:           b.ID,
		Conversation: d.required(keyRTConversation),
		Closed:       b.Status == beadStatusClosed,
		Generation:   d.int64(keyRTGen),
		Epoch:        d.int64(keyRTEpoch),
		Seq:          d.uint64(keyRTSeq),
		Turn: TurnCells{
			Generation: d.int64(keyRTTurnGen),
			ID:         d.meta[keyRTTurnID],
			State:      TurnState(d.meta[keyRTTurnState]),
			At:         d.time(keyRTTurnAt),
			LastID:     d.meta[keyRTTurnLastID],
			LastEnd:    TurnEndReason(d.meta[keyRTTurnLastEnd]),
			LastAt:     d.time(keyRTTurnLastAt),
			Seen:       d.meta[keyRTTurnSeen],
			ObservedAt: d.time(keyRTTurnObservedAt),
		},
		State:      RuntimeState(d.meta[keyRTState]),
		StateAt:    d.time(keyRTStateAt),
		Attached:   Attachment(d.meta[keyRTAttached]),
		AttachedAt: d.time(keyRTAttachedAt),
		ActivityAt: d.time(keyRTActivityAt),
		Observer:   d.meta[keyRTObserver],
		Binding: RuntimeBinding{
			Thread:          d.meta[keyRTBindingThread],
			Project:         d.meta[keyRTBindingProject],
			ProviderSession: d.meta[keyRTBindingProviderSession],
		},
		Host: HostStamp{
			Host:       d.meta[keyRTHost],
			Generation: d.int64(keyRTHostGen),
			Protocol:   d.meta[keyRTHostProtocol],
			Build:      d.meta[keyRTHostBuild],
			Since:      d.time(keyRTHostSince),
		},
	}
	if d.err != nil {
		return RuntimeRow{}, d.err
	}
	if lease := d.meta[keyRTLease]; lease != "" {
		l, err := ParseLease(lease)
		if err != nil {
			return RuntimeRow{}, corruptf(b.ID, "%s: %v", keyRTLease, err)
		}
		r.Lease = l
	}
	caps, err := ParseTurnCapsStamp(d.meta[keyRTTurnCaps])
	if err != nil {
		return RuntimeRow{}, corruptf(b.ID, "%s: %v", keyRTTurnCaps, err)
	}
	r.Turn.Caps = caps
	if kinds := d.meta[keyRTHostKinds]; kinds != "" {
		for _, k := range strings.Split(kinds, ",") {
			r.Host.Kinds = append(r.Host.Kinds, Kind(k))
		}
	}
	for k, v := range b.Metadata {
		if !slices.Contains(runtimeKeys, k) && v != "" {
			if r.Extra == nil {
				r.Extra = map[string]string{}
			}
			r.Extra[k] = v
		}
	}
	if err := r.validateShape(); err != nil {
		return RuntimeRow{}, corruptf(b.ID, "%v", err)
	}
	if !slices.Contains(b.Labels, RuntimeConversationLabel(r.Conversation)) {
		return RuntimeRow{}, corruptf(b.ID, "missing label %s", RuntimeConversationLabel(r.Conversation))
	}
	prefix, ok := strings.CutSuffix(b.ID, runtimeIDInfix+runtimeIDSuffix(r.Conversation))
	if !ok {
		return RuntimeRow{}, corruptf(b.ID, "id does not match its conversation")
	}
	if err := validatePrefix(prefix); err != nil {
		return RuntimeRow{}, corruptf(b.ID, "id prefix: %v", err)
	}
	return r, nil
}

// RuntimeRowPatch is the update that turns row b into next's encoding: the
// owned keys that changed, an owned key written empty where b has it and next
// does not, and the bead status when next closes the row. It never writes a
// key this build does not own, whatever next.Extra holds, so a newer build's
// cells survive. A patch that moves the row to another conversation or
// reopens it is refused, as is free text a fenced update would not store as a
// string (bd types a value such as 42, true or null by its spelling), and an
// observation value this build does not know where the patch writes it; one
// already stored and left unchanged is kept. The caller writes the patch
// under b's revision.
//
// Build next from DecodeRuntimeRow(b) and change only the cells being
// written: the patch compares next with every owned key of b, so a next
// built from scratch writes empty over each cell it leaves zero, a newer
// build's observation value included.
func RuntimeRowPatch(b beads.Bead, next RuntimeRow) (beads.UpdateOpts, error) {
	cur, err := DecodeRuntimeRow(b)
	if err != nil {
		return beads.UpdateOpts{}, err
	}
	if next.Conversation != cur.Conversation || (next.ID != "" && next.ID != cur.ID) {
		return beads.UpdateOpts{}, invalidf("runtime row %s belongs to conversation %s", cur.ID, cur.Conversation)
	}
	if cur.Closed && !next.Closed {
		return beads.UpdateOpts{}, invalidf("runtime row %s is closed and does not reopen", cur.ID)
	}
	next.Extra = nil
	if err := next.validateShape(); err != nil {
		return beads.UpdateOpts{}, err
	}
	want := next.ownedFields()
	meta := map[string]string{}
	for _, k := range runtimeKeys {
		if b.Metadata[k] != want[k] {
			meta[k] = want[k]
		}
	}
	if err := next.validateVocabulary(func(k string) bool { _, ok := meta[k]; return ok }); err != nil {
		return beads.UpdateOpts{}, err
	}
	for _, k := range updateTextKeys {
		if v, ok := meta[k]; ok && v != "" {
			if err := validateUpdateText(k, v); err != nil {
				return beads.UpdateOpts{}, err
			}
		}
	}
	var opts beads.UpdateOpts
	if len(meta) > 0 {
		opts.Metadata = meta
	}
	if next.Closed && !cur.Closed {
		status := beadStatusClosed
		opts.Status = &status
	}
	return opts, nil
}
