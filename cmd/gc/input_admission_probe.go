package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/inputadmission"
	"github.com/gastownhall/gascity/internal/readywork"
)

// inputAdmissionProbeDeadline bounds one probe judgment of input admission.
// It sits under the v2 fence's own probe bound (fenceProbeTimeout), so a slow
// input read leaves the fence's work leg an answer instead of an expiry.
const inputAdmissionProbeDeadline = 3 * time.Second

// errInputAdmissionProbeDeadline: a probe's judgment outlived
// inputAdmissionProbeDeadline.
var errInputAdmissionProbeDeadline = errors.New("input admission judgment exceeded its deadline")

// inputAdmissionProbe is one controller read's input-admission judge for the
// probes that decide a seat's fate from its live work (#8832): keep-awake, the
// drain-ack strand proof, park and the work ladder. It judges over the city's
// planned ready legs (WorkLegs.inputJudge), the stores the hook's claim and
// the demand gate read, so a split city's root -> input convoy -> members
// relation, which spans the binding and the work store, is seen whole: a probe
// that judged one leg alone counted as work a step the hook refuses, and
// held, kept awake or nudged a seat for it (the ga-5736js loop).
//
// It reads nothing until a probe asks, then memoizes every root, convoy and
// stopped-workflow verdict for its life: one seat-work read (SeatWork) or one
// ladder pass. A judgment that fails or outlives inputAdmissionProbeDeadline
// withholds nothing, the never-under-count rule the demand gate applies: the
// probe keeps its ungated answer, the failure is logged once, and the probe
// judges nothing more, so no later probe waits on a read that already stalled.
// A nil probe judges nothing.
type inputAdmissionProbe struct {
	legs WorkLegs
	now  func() time.Time

	once  sync.Once
	judge *readywork.InputJudge

	mu  sync.Mutex
	err error
}

// newInputAdmissionProbe returns the probe over legs, judging deferral at the
// wall clock when it first judges.
func newInputAdmissionProbe(legs WorkLegs) *inputAdmissionProbe {
	return &inputAdmissionProbe{legs: legs, now: time.Now}
}

// withheld returns the ids of rows, read from store, that input admission
// withholds, or nil when it withholds none or cannot judge them.
func (p *inputAdmissionProbe) withheld(store beads.StoreReader, rows []beads.Bead) map[string]bool {
	if p == nil || !anyInputAdmissionSubject(rows) {
		return nil
	}
	p.once.Do(func() {
		judge, err := p.legs.inputJudge(p.now())
		p.judge = judge
		p.fail(err)
	})
	if p.failed() {
		return nil
	}
	type judged struct {
		verdicts map[string]inputadmission.Withhold
		err      error
	}
	ctx, cancel := context.WithTimeout(context.Background(), inputAdmissionProbeDeadline)
	defer cancel()
	done := make(chan judged, 1)
	go func() {
		verdicts, err := guardedRead(func() (map[string]inputadmission.Withhold, error) {
			return p.judge.Withheld(ctx, store, rows)
		})
		done <- judged{verdicts, err}
	}()
	var j judged
	select {
	case j = <-done:
	case <-ctx.Done():
		j.err = fmt.Errorf("%w (%s)", errInputAdmissionProbeDeadline, inputAdmissionProbeDeadline)
	}
	if j.err != nil {
		p.fail(j.err)
		return nil
	}
	if len(j.verdicts) == 0 {
		return nil
	}
	out := make(map[string]bool, len(j.verdicts))
	for id := range j.verdicts {
		out[id] = true
	}
	return out
}

// fail records the probe's first failure and logs it; a nil err records
// nothing.
func (p *inputAdmissionProbe) fail(err error) {
	if err == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return
	}
	p.err = err
	log.Printf("input admission probe: judging nothing more for this read, seat probes keep their ungated answer: %v", err)
}

// failed reports whether a judgment already failed.
func (p *inputAdmissionProbe) failed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err != nil
}

// anyInputAdmissionSubject reports whether input admission judges any of rows
// (inputadmission.Subject): a probe over none reads nothing.
func anyInputAdmissionSubject(rows []beads.Bead) bool {
	for _, row := range rows {
		if inputadmission.Subject(row) {
			return true
		}
	}
	return false
}
