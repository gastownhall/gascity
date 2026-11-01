package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The program-status shadow recorder (design §9.3): the one adapter every
// shadow decision point in a gc process hands its comparisons to, the tmux
// leaf's (through tmux.Config.ComparisonSink) and every point outside it
// (through runtime.ShadowCompare, which finds the same sink on the leaf).
//
// A comparison is offered without ever blocking the decision path: a
// continuously evaluated point's comparison is kept only when its (scrape,
// program, validity) triple for the session changed, then pushed onto a
// bounded per-city queue. A full queue drops the newest comparison and
// counts it into the next recorded payload's dropped, as are the drops a
// comparison carries when the change filter does not record it. One
// goroutine per city drains the queue into the city's events log as
// session.program_status_shadow, opening the recorder lazily on the first
// comparison and keeping it for the process.
//
// Only a long-lived process (the supervisor, a foreground controller:
// markProgramShadowLongLived) follows a timed comparison past its decision.
// A short-lived CLI process records each decision-time verdict untimed, so
// nothing it decides waits on a follow-up it may exit before; at exit it
// waits, bounded by programShadowFlushTimeout, for the leaf's background
// work still judging a comparison and then for its queues.

const (
	// programShadowQueueSize bounds each city's comparison queue.
	programShadowQueueSize = 256
	// programShadowFlushTimeout bounds the exit flush of a CLI process.
	programShadowFlushTimeout = time.Second
	// programShadowFlushPoll is how often a flush checks the queue drained.
	programShadowFlushPoll = 5 * time.Millisecond
	// programShadowChangeKeys bounds the change memory; past it the memory
	// starts over, which can only record one comparison more per key.
	programShadowChangeKeys = 8192
)

// programShadowRecorders holds each city's recorder, by city path.
var programShadowRecorders sync.Map

// programShadowLongLived is set by a process that runs for the life of a
// city (markProgramShadowLongLived): only such a process follows timed
// comparisons past their decision.
var programShadowLongLived atomic.Bool

// programShadowWork counts the leaf's background shadow work in flight in
// this process: comparisons still being judged, not yet offered.
var programShadowWork atomic.Int64

// markProgramShadowLongLived declares the process long-lived: the
// supervisor and a foreground controller call it before they serve a city.
func markProgramShadowLongLived() { programShadowLongLived.Store(true) }

// programShadowFollowUps reports whether this process follows timed
// comparisons past their decision (tmux Config.ComparisonFollowUps).
func programShadowFollowUps() bool { return programShadowLongLived.Load() }

// programShadowBeginWork counts one piece of the leaf's background shadow
// work and returns the call that ends it (tmux Config.ComparisonWork).
func programShadowBeginWork() func() {
	programShadowWork.Add(1)
	var once sync.Once
	return func() { once.Do(func() { programShadowWork.Add(-1) }) }
}

// programShadowOpen opens the events recorder comparisons are written to. A
// var so tests read the events back.
var programShadowOpen = func(cityPath string) events.Recorder {
	rec, err := openCityEventsLog(cityPath, io.Discard)
	if err != nil {
		// The shadow's data is lost while this lasts: say so once, where
		// the process's operator reads.
		fmt.Fprintf(os.Stderr, "gc: program-status shadow: cannot open the events log of %s, comparisons are dropped: %v\n", cityPath, err) //nolint:errcheck // best-effort stderr
		return events.Discard
	}
	return rec
}

// programShadowKey is one session's decision point.
type programShadowKey struct {
	session string
	point   runtime.ProgramStatusPoint
}

// programShadowReading is what a change point's comparison is compared on.
type programShadowReading struct {
	scrape, program runtime.ProgramVerdict
	validity        runtime.ProgramStatusValidity
}

// programShadowRecorder is one city's shadow recorder.
type programShadowRecorder struct {
	cityPath string
	queue    chan runtime.ProgramStatusComparison
	dropped  atomic.Int64
	inflight atomic.Int64
	start    sync.Once

	mu   sync.Mutex
	last map[programShadowKey]programShadowReading
}

// programStatusShadowSink returns the comparison sink for the city at
// cityPath: the process's one recorder for that city.
func programStatusShadowSink(cityPath string) runtime.ProgramStatusComparisonSink {
	r, _ := programShadowRecorders.LoadOrStore(cityPath, &programShadowRecorder{
		cityPath: cityPath,
		queue:    make(chan runtime.ProgramStatusComparison, programShadowQueueSize),
		last:     make(map[programShadowKey]programShadowReading),
	})
	return r.(*programShadowRecorder).offer
}

// offer takes one comparison without blocking.
func (r *programShadowRecorder) offer(c runtime.ProgramStatusComparison) {
	r.start.Do(func() { go r.drain() })
	r.mu.Lock()
	defer r.mu.Unlock()
	key := programShadowKey{session: c.Session, point: c.Point}
	reading := programShadowReading{scrape: c.Scrape, program: c.Program, validity: c.Status.Validity}
	if c.Point.RecordsOnChange() {
		if prev, ok := r.last[key]; ok && prev == reading {
			// The comparison is not recorded; the drops it carries are.
			r.dropped.Add(int64(c.Dropped))
			return
		}
	}
	r.inflight.Add(1)
	select {
	case r.queue <- c:
		if c.Point.RecordsOnChange() {
			if len(r.last) >= programShadowChangeKeys {
				r.last = make(map[programShadowKey]programShadowReading)
			}
			r.last[key] = reading
		}
	default:
		r.inflight.Add(-1)
		r.dropped.Add(int64(1 + c.Dropped))
	}
}

// drain writes queued comparisons to the city's events log for the life of
// the process.
func (r *programShadowRecorder) drain() {
	var rec events.Recorder
	for c := range r.queue {
		rec = r.write(rec, c)
	}
}

// write writes one queued comparison through rec, opening it on first use,
// and returns the recorder for the next. A panic while writing is contained
// (a supervisor process hosts several cities) and the comparison counted as
// dropped, so the drain keeps serving the city.
func (r *programShadowRecorder) write(rec events.Recorder, c runtime.ProgramStatusComparison) (out events.Recorder) {
	out = rec
	defer r.inflight.Add(-1)
	defer func() {
		if recover() != nil {
			r.dropped.Add(1)
		}
	}()
	if out == nil {
		out = programShadowOpen(r.cityPath)
	}
	dropped := int(r.dropped.Swap(0)) + c.Dropped
	if ev, ok := programShadowEvent(c, dropped, time.Now()); ok {
		out.Record(ev)
	}
	return out
}

// flush waits, up to timeout, for every comparison offered so far to be
// written. It reports whether they all were.
func (r *programShadowRecorder) flush(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for r.inflight.Load() > 0 {
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(programShadowFlushPoll)
	}
	return true
}

// flushProgramStatusShadow waits for the leaf's background shadow work and
// then flushes every city's shadow recorder, bounded by timeout overall. A
// process that never shadowed returns at once. Work still judging a
// comparison at the deadline (a long-lived process's follow-up) is lost;
// the count is said on stderr, where the process's operator reads.
func flushProgramStatusShadow(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for programShadowWork.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(programShadowFlushPoll)
	}
	if cut := programShadowWork.Load(); cut > 0 {
		fmt.Fprintf(os.Stderr, "gc: program-status shadow: %d comparisons still being judged at exit are not recorded\n", cut) //nolint:errcheck // best-effort stderr
	}
	programShadowRecorders.Range(func(_, v any) bool {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		v.(*programShadowRecorder).flush(remaining)
		return true
	})
}

// shadowHold records a hold point's comparison (execution_backstop,
// claim_nudge_hold, work_ladder, queued_delivery, drain_reminder) for sp's
// session sessName: held is today's verdict, rule the record's, judged with
// the runtime's last activity at now; the comparison carries both, so its
// hold_live is the rule's own. It does nothing unless the leaf serving the
// session is shadowing, and never feeds the decision.
func shadowHold(sp runtime.Provider, sessName, sessionID, family string, point runtime.ProgramStatusPoint, held bool, now time.Time, rule func(runtime.ProgramStatus, time.Time, time.Time) runtime.ProgramVerdict) {
	scrape := runtime.ProgramVerdictNoHold
	if held {
		scrape = runtime.ProgramVerdictHold
	}
	runtime.ShadowCompareHold(sp, sessName, sessionID, family, point, scrape, now, rule)
}

// programShadowFamily is the shape a family token must have to be recorded:
// a provider family is config data, but it is still kept to a short closed
// alphabet so no free text reaches the payload.
var programShadowFamily = regexp.MustCompile(`^[a-z0-9][a-z0-9_.+-]{0,31}$`)

// programShadowPayload is c's typed payload recorded at now, carrying
// dropped. The record's age and hold liveness are the ones its rule judged
// with, at c's judged time: a timed point records after its follow-up, which
// can end minutes after the decision.
func programShadowPayload(c runtime.ProgramStatusComparison, dropped int, now time.Time) events.SessionProgramStatusShadowPayload {
	s := c.Status
	p := events.SessionProgramStatusShadowPayload{
		SessionID:      c.SessionID,
		Point:          string(c.Point),
		Scrape:         string(c.Scrape),
		Program:        string(c.Program),
		Agree:          c.Scrape == c.Program,
		Validity:       string(s.Validity),
		HoldLive:       c.HoldLive(now),
		ReaderVerified: s.ReaderVerified,
		Dropped:        dropped,
	}
	if family := strings.ToLower(strings.TrimSpace(c.Family)); family != "" {
		p.Family = "other"
		if programShadowFamily.MatchString(family) {
			p.Family = family
		}
	}
	if s.Validity == runtime.ProgramStatusUnread {
		p.Validity = "unread"
	}
	if s.Valid() {
		p.State = string(s.State)
		if s.Blocked() {
			p.Kind = string(s.Kind)
		}
		if judged := c.JudgedTime(now); !s.Since.IsZero() && judged.After(s.Since) {
			p.RecordAgeMs = judged.Sub(s.Since).Milliseconds()
		}
	}
	if c.Timed {
		p.Timed, p.LatencyMs = true, c.LatencyMs
	}
	return p
}

// programShadowEvent is c's session.program_status_shadow event at now. The
// envelope names the session (subject: its runtime name) and carries no
// message text.
func programShadowEvent(c runtime.ProgramStatusComparison, dropped int, now time.Time) (events.Event, bool) {
	payload, err := json.Marshal(programShadowPayload(c, dropped, now))
	if err != nil {
		return events.Event{}, false
	}
	return events.Event{
		Type:      events.SessionProgramStatusShadow,
		Actor:     eventActor(),
		Subject:   c.Session,
		SessionID: c.SessionID,
		Payload:   payload,
	}, true
}
