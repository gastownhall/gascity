package main

import (
	"io"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

type (
	sessionReconcilerTraceManager = SessionReconcilerTracer
	sessionReconcilerTraceCycle   = SessionReconcilerTraceCycle
)

func newSessionReconcilerTraceManager(cityPath, cityName string, stderr io.Writer) *sessionReconcilerTraceManager {
	return newSessionReconcilerTracer(cityPath, cityName, stderr)
}

func (m *SessionReconcilerTracer) beginCycle(info sessionReconcilerTraceCycleInfo, cfg *config.City, sessionBeads *sessionBeadSnapshot) *SessionReconcilerTraceCycle {
	if m == nil {
		return nil
	}
	cycle := m.BeginCycle(TraceTickTrigger(info.TickTrigger), info.TriggerDetail, time.Now().UTC(), cfg)
	if cycle != nil {
		cycle.configRevision = info.ConfigRevision
	}
	if cycle != nil && sessionBeads != nil {
		cycle.RecordSessionBaseline("", "", traceRecordPayload{
			"open_count": len(sessionBeads.OpenInfos()),
		})
		_ = cycle.flushCurrentBatch(TraceDurabilityDurable)
	}
	return cycle
}

func (c *SessionReconcilerTraceCycle) detailEnabled(template string) bool {
	if c == nil {
		return false
	}
	_, ok := c.detailSource(template)
	return ok
}

func (c *SessionReconcilerTraceCycle) sourceFor(template string) string {
	if c == nil {
		return string(TraceSourceAlwaysOn)
	}
	if source, ok := c.detailSource(template); ok {
		return source
	}
	return string(TraceSourceAlwaysOn)
}

// RecordControllerDecision records a baseline daemon-level decision that is
// not scoped to a specific session template.
func (c *SessionReconcilerTraceCycle) RecordControllerDecision(site TraceSiteCode, reason TraceReasonCode, outcome TraceOutcomeCode, fields map[string]any) {
	if c == nil {
		return
	}
	rec := newTraceRecord(TraceRecordDecision).withCycle(c, time.Now().UTC())
	rec.SiteCode = site
	rec.ReasonCode = reason
	rec.OutcomeCode = outcome
	rec.TraceMode = TraceModeBaseline
	rec.TraceSource = TraceSourceAlwaysOn
	if len(fields) > 0 {
		rec.ensureFields()
		for k, v := range fields {
			rec.Fields[k] = v
		}
	}
	c.addRecord(rec)
}

// RecordControllerOperation records an always-on controller phase duration.
func (c *SessionReconcilerTraceCycle) RecordControllerOperation(site TraceSiteCode, reason TraceReasonCode, outcome TraceOutcomeCode, opName string, duration time.Duration, fields map[string]any) {
	if c == nil {
		return
	}
	rec := newTraceRecord(TraceRecordOperation).withCycle(c, time.Now().UTC())
	rec.SiteCode = site
	rec.ReasonCode = reason
	rec.OutcomeCode = outcome
	rec.OperationID = newTraceID(opName)
	rec.TraceMode = TraceModeBaseline
	rec.TraceSource = TraceSourceAlwaysOn
	rec.DurationMS = duration.Milliseconds()
	rec.ensureFields()
	rec.Fields["operation_name"] = opName
	for k, v := range fields {
		rec.Fields[k] = v
	}
	c.addRecord(rec)
}

func (c *SessionReconcilerTraceCycle) recordAdmittedDetailOperation(
	site TraceSiteCode,
	reason TraceReasonCode,
	outcome TraceOutcomeCode,
	opName string,
	template string,
	sessionBeadID string,
	sessionName string,
	source TraceSource,
	duration time.Duration,
	fields map[string]any,
) {
	c.recordSessionOperation(TraceModeDetail, source, site, reason, outcome, opName, template, sessionBeadID, sessionName, duration, fields)
}

// recordKeyedEffect records one keyed handler's outcome for a session, tiering
// it on whether the handler actually committed the effect.
//
// An APPLIED effect is the load-bearing proof that the opt-in keyed engine acted
// on a row, and an unarmed city — the shipping default — is exactly where that
// proof is needed and exactly where it used to be discarded. So an applied
// effect persists on the always-on tier whatever the arming state, the same
// treatment pool_allocation.materialize already gets from
// RecordControllerOperation.
//
// Everything else stays detail-gated, because volume is what the gate is for:
// the 2026-08-24 soak census counted 59,486 detail-gated condition records in
// the window (59,447 dropped) against a handful of applied effects, so the
// always-on tier only ever pays for the handful. Decisions, refusals, yields and
// the detector shadow are unchanged — in particular this seam never arms
// anything, so the detector-shadow vocabulary still cannot auto-arm.
//
// When detail IS armed the record keeps its detail tier verbatim, so an armed
// city sees exactly what it saw before.
func (c *SessionReconcilerTraceCycle) recordKeyedEffect(
	site TraceSiteCode,
	reason TraceReasonCode,
	outcome TraceOutcomeCode,
	opName string,
	template string,
	sessionBeadID string,
	sessionName string,
	duration time.Duration,
	fields map[string]any,
) {
	if c == nil {
		return
	}
	if source, armed := c.detailSource(template); armed {
		c.recordSessionOperation(TraceModeDetail, TraceSource(source), site, reason, outcome, opName, template, sessionBeadID, sessionName, duration, fields)
		return
	}
	if applied, _ := fields["effect_applied"].(bool); !applied {
		return
	}
	c.recordSessionOperation(TraceModeBaseline, TraceSourceAlwaysOn, site, reason, outcome, opName, template, sessionBeadID, sessionName, duration, fields)
}

func (c *SessionReconcilerTraceCycle) recordSessionOperation(
	mode TraceMode,
	source TraceSource,
	site TraceSiteCode,
	reason TraceReasonCode,
	outcome TraceOutcomeCode,
	opName string,
	template string,
	sessionBeadID string,
	sessionName string,
	duration time.Duration,
	fields map[string]any,
) {
	if c == nil {
		return
	}
	rec := newTraceRecord(TraceRecordOperation).withCycle(c, time.Now().UTC())
	rec.SiteCode = site
	rec.ReasonCode = reason
	rec.OutcomeCode = outcome
	rec.Template = normalizedTraceTemplate(template)
	rec.SessionBeadID = sessionBeadID
	rec.SessionName = sessionName
	rec.OperationID = newTraceID(opName)
	rec.TraceMode = mode
	rec.TraceSource = source
	rec.DurationMS = duration.Milliseconds()
	rec.ensureFields()
	rec.Fields["operation_name"] = opName
	for k, v := range fields {
		rec.Fields[k] = v
	}
	c.addRecord(rec)
}

func (c *SessionReconcilerTraceCycle) end(completion TraceCompletionStatus, data traceRecordPayload) {
	if c == nil {
		return
	}
	fields := make(map[string]any, len(data))
	for k, v := range data {
		fields[k] = v
	}
	_ = c.End(completion, fields)
}
