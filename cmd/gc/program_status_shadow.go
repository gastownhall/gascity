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
// counts it into the next recorded payload's dropped. One goroutine per city
// drains the queue into the city's events log as
// session.program_status_shadow, opening the recorder lazily on the first
// comparison and keeping it for the process. A CLI process flushes the
// queue at exit, bounded by programShadowFlushTimeout.

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
		if rec == nil {
			rec = programShadowOpen(r.cityPath)
		}
		dropped := int(r.dropped.Swap(0)) + c.Dropped
		if ev, ok := programShadowEvent(c, dropped, time.Now()); ok {
			rec.Record(ev)
		}
		r.inflight.Add(-1)
	}
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

// flushProgramStatusShadow flushes every city's shadow recorder, bounded by
// timeout overall. A process that never shadowed returns at once.
func flushProgramStatusShadow(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
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
// the runtime's last activity at now. It does nothing unless the leaf serving
// the session is shadowing, and never feeds the decision.
func shadowHold(sp runtime.Provider, sessName, sessionID, family string, point runtime.ProgramStatusPoint, held bool, now time.Time, rule func(runtime.ProgramStatus, time.Time, time.Time) runtime.ProgramVerdict) {
	scrape := runtime.ProgramVerdictNoHold
	if held {
		scrape = runtime.ProgramVerdictHold
	}
	runtime.ShadowCompare(sp, sessName, sessionID, family, point, scrape, func(s runtime.ProgramStatus) runtime.ProgramVerdict {
		// An unreadable activity clock leaves the hold bound on the record's
		// own age, which is what a zero lastActivity means to HoldLive.
		last, err := sp.GetLastActivity(sessName)
		if err != nil {
			last = time.Time{}
		}
		return rule(s, last, now)
	})
}

// programShadowFamily is the shape a family token must have to be recorded:
// a provider family is config data, but it is still kept to a short closed
// alphabet so no free text reaches the payload.
var programShadowFamily = regexp.MustCompile(`^[a-z0-9][a-z0-9_.+-]{0,31}$`)

// programShadowPayload is c's typed payload at now, carrying dropped.
func programShadowPayload(c runtime.ProgramStatusComparison, dropped int, now time.Time) events.SessionProgramStatusShadowPayload {
	s := c.Status
	p := events.SessionProgramStatusShadowPayload{
		SessionID:      c.SessionID,
		Point:          string(c.Point),
		Scrape:         string(c.Scrape),
		Program:        string(c.Program),
		Agree:          c.Scrape == c.Program,
		Validity:       string(s.Validity),
		HoldLive:       s.HoldLive(time.Time{}, now),
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
		if !s.Since.IsZero() && now.After(s.Since) {
			p.RecordAgeMs = now.Sub(s.Since).Milliseconds()
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
