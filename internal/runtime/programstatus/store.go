package programstatus

import (
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// MaxChildren bounds the child records a Store keeps; at the bound the least
// recently updated child is evicted.
const MaxChildren = 64

// Text is a record's free text: never carried in [runtime.ProgramStatus],
// published separately ([EncodeText]).
type Text struct {
	Title        string // the root record's title
	Msg          string // the root record's msg
	LastErrorMsg string // the msg of the last root error report; sticky across clear
}

// record is one stored OSC 7501 record.
type record struct {
	state    runtime.ProgramState
	kind     runtime.ProgramBlockKind
	progress int
	app      string
	title    string
	msg      string
	touched  uint64 // update order, for eviction
}

// Store keeps one capture incarnation's OSC 7501 records with spec rev 0.4
// semantics and the root record's turn accounting:
//
//   - a report replaces its record; clear drops the addressed record and its
//     descendants, or every record when it has no id;
//   - a prompt start (OSC 133;A) drops working and blocked records without
//     counting a turn end;
//   - TurnStarts counts the root entering working from a boundary (idle, done
//     or error) or from no root; blocked pauses a turn without ending it, so
//     blocked→working is not a start;
//   - TurnEnds counts the root leaving working or blocked for a boundary,
//     and stamps QuietSince;
//   - Since moves only when the root's state changes;
//   - LastError, set by a root error report, survives clear;
//   - Seq increments with every change to the encoded record ([Encode]).
//
// Children never start or end the root's turn. A Store is not safe for
// concurrent use.
type Store struct {
	nonce      string
	records    map[string]*record // "" is the root
	clock      uint64
	since      time.Time
	quietSince time.Time
	turnStarts uint32
	turnEnds   uint32
	probed     bool
	lastError  bool
	lastErrMsg string
	seq        uint64
	encoded    string // fields of the last status, Seq excluded
}

// NewStore returns an empty Store for the capture incarnation nonce
// ("<rand>.<pane_id>").
func NewStore(nonce string) *Store {
	s := &Store{nonce: nonce, records: map[string]*record{}}
	s.encoded = s.fields()
	return s
}

// Apply applies one validated report at now (the capture host clock).
func (s *Store) Apply(r Report, now time.Time) {
	prev := s.rootState()
	switch {
	case r.Clear && r.ID == "":
		clear(s.records)
	case r.Clear:
		s.dropSubtree(r.ID)
	default:
		s.put(r)
	}
	if r.ID == "" && r.State == runtime.ProgramError {
		s.lastError, s.lastErrMsg = true, r.Msg
	}
	s.rootChanged(prev, now)
	s.bump()
}

// PromptStart applies OSC 133;A: working and blocked records are dropped, and
// a dropped root counts no turn end.
func (s *Store) PromptStart() {
	prev := s.rootState()
	for id, rec := range s.records {
		if busy(rec.state) {
			delete(s.records, id)
		}
	}
	s.rootChanged(prev, time.Time{})
	s.bump()
}

// Probe records that the program sent the support probe.
func (s *Store) Probe() {
	s.probed = true
	s.bump()
}

// Status returns the root record and counters as an unjudged reading:
// Mode, Validity and ReaderVerified are left for the reader to judge.
func (s *Store) Status() runtime.ProgramStatus {
	st := runtime.ProgramStatus{
		Nonce:      s.nonce,
		Seq:        s.seq,
		Progress:   -1,
		Since:      s.since,
		QuietSince: s.quietSince,
		TurnStarts: s.turnStarts,
		TurnEnds:   s.turnEnds,
		Probed:     s.probed,
		LastError:  s.lastError,
	}
	for id, rec := range s.records {
		if id == "" {
			st.State, st.Kind, st.Progress, st.App = rec.state, rec.kind, rec.progress, rec.app
			continue
		}
		st.Children++
		if rec.state == runtime.ProgramWorking {
			st.WorkingChildren++
		}
	}
	return st
}

// Text returns the root record's title and msg and the sticky last error
// message.
func (s *Store) Text() Text {
	t := Text{LastErrorMsg: s.lastErrMsg}
	if root, ok := s.records[""]; ok {
		t.Title, t.Msg = root.title, root.msg
	}
	return t
}

// EffectiveApp returns the app r would carry: its own, else the app of its
// nearest stored ancestor that has one. "" means no app anywhere.
func (s *Store) EffectiveApp(r Report) string {
	if r.App != "" || r.ID == "" {
		return r.App
	}
	id := r.ID
	for id != "" {
		cut := strings.LastIndexByte(id, '/')
		if cut < 0 {
			id = ""
		} else {
			id = id[:cut]
		}
		if rec, ok := s.records[id]; ok && rec.app != "" {
			return rec.app
		}
	}
	return ""
}

func (s *Store) rootState() runtime.ProgramState {
	if root, ok := s.records[""]; ok {
		return root.state
	}
	return ""
}

func (s *Store) put(r Report) {
	s.clock++
	if _, ok := s.records[r.ID]; !ok && r.ID != "" && s.children() >= MaxChildren {
		s.evictOldestChild()
	}
	s.records[r.ID] = &record{
		state:    r.State,
		kind:     r.Kind,
		progress: r.Progress,
		app:      r.App,
		title:    r.Title,
		msg:      r.Msg,
		touched:  s.clock,
	}
}

func (s *Store) dropSubtree(id string) {
	prefix := id + "/"
	for k := range s.records {
		if k == id || strings.HasPrefix(k, prefix) {
			delete(s.records, k)
		}
	}
}

func (s *Store) children() int {
	n := len(s.records)
	if _, ok := s.records[""]; ok {
		n--
	}
	return n
}

func (s *Store) evictOldestChild() {
	oldest := ""
	var at uint64
	for id, rec := range s.records {
		if id != "" && (oldest == "" || rec.touched < at) {
			oldest, at = id, rec.touched
		}
	}
	delete(s.records, oldest)
}

// rootChanged applies the turn rules to a root transition from prev.
func (s *Store) rootChanged(prev runtime.ProgramState, now time.Time) {
	next := s.rootState()
	if next == prev {
		return
	}
	if next == "" {
		s.since = time.Time{}
		return
	}
	s.since = now
	if next == runtime.ProgramWorking && (prev == "" || boundary(prev)) {
		s.turnStarts++
	}
	if busy(prev) && boundary(next) {
		s.turnEnds++
		s.quietSince = now
	}
}

// bump increments Seq when the encoded record changed.
func (s *Store) bump() {
	if f := s.fields(); f != s.encoded {
		s.encoded = f
		s.seq++
	}
}

// fields is the encoded record with Seq held at zero.
func (s *Store) fields() string {
	st := s.Status()
	st.Seq = 0
	return encodeFields(st)
}

func busy(st runtime.ProgramState) bool {
	return st == runtime.ProgramWorking || st == runtime.ProgramBlocked
}

func boundary(st runtime.ProgramState) bool {
	return st == runtime.ProgramIdle || st == runtime.ProgramDone || st == runtime.ProgramError
}
