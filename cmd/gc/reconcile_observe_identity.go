package main

import (
	"fmt"
	"time"
)

// Arm A3's alerts (C4c2 review; OBS1's once-per-episode alerts): a row whose
// rekey keeps being refused, and a row held NewerSelf past a dwell. Each
// alerts once per episode, and re-arms when its condition clears.

const (
	alertRekeyRefused  = "rekey-refused"   // rekeyRefusalsAlert consecutive refused or failed rekeys for a row
	alertNewerSelfHeld = "newer-self-held" // a row held NewerSelf longer than newerSelfHeldAlert

	rekeyRefusalsAlert = 5
	newerSelfHeldAlert = 10 * time.Minute
)

// identityObserver is A3's alert memory.
type identityObserver struct {
	refusals     map[rowKey]int       // consecutive refused or failed rekeys
	newerSince   map[rowKey]time.Time // the first pass that held the row NewerSelf
	newerAlerted map[rowKey]bool
}

// observeRekeySettlement counts a drained rekey settlement: a refusal or a
// failure (a write-error, a deadline) extends the row's streak, alerting
// once when it reaches rekeyRefusalsAlert; any other outcome ends it.
func (p *planner) observeRekeySettlement(s settlement) {
	if s.Kind != intentRekey {
		return
	}
	o := &p.obs.identity
	if s.Outcome != settledRefused && s.Outcome != settledFailed {
		delete(o.refusals, s.Key)
		return
	}
	if o.refusals == nil {
		o.refusals = make(map[rowKey]int)
	}
	if o.refusals[s.Key]++; o.refusals[s.Key] == rekeyRefusalsAlert {
		p.alert(alertRekeyRefused, s.Key.ID, fmt.Sprintf("session %s on leg %q: %d consecutive rekeys refused or failed, the last with cause %s", s.Key.ID, s.Key.Leg, rekeyRefusalsAlert, s.Cause))
	}
}

// observeIdentityHolds reads the pass's decide reasons (rows with an effect
// in flight have none, and keep their state). A row that decided anything
// but a rekey ends its refusal streak; a row held NewerSelf alerts once past
// newerSelfHeldAlert, and re-arms once it decides anything else. A row gone
// from the census forgets both.
func (p *planner) observeIdentityHolds(w *World, reasons map[rowKey]string) {
	o := &p.obs.identity
	ended := func(k rowKey, keep string) bool {
		reason, decided := reasons[k]
		_, open := w.Census.Rows[k]
		return !open || decided && reason != keep
	}
	for k := range o.refusals {
		if ended(k, decideRekey) {
			delete(o.refusals, k)
		}
	}
	for k := range o.newerSince {
		if ended(k, decideNewerSelf) {
			delete(o.newerSince, k)
			delete(o.newerAlerted, k)
		}
	}
	for k, reason := range reasons {
		if reason != decideNewerSelf {
			continue
		}
		if o.newerSince == nil {
			o.newerSince, o.newerAlerted = make(map[rowKey]time.Time), make(map[rowKey]bool)
		}
		since, held := o.newerSince[k]
		if !held {
			o.newerSince[k] = w.Now
			continue
		}
		if w.Now.Sub(since) > newerSelfHeldAlert && !o.newerAlerted[k] {
			o.newerAlerted[k] = true
			p.alert(alertNewerSelfHeld, k.ID, fmt.Sprintf("session %s on leg %q: its runtime has read NewerSelf (census lag) since %s", k.ID, k.Leg, since.UTC().Format(time.RFC3339)))
		}
	}
}
