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
