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
			"open_count": len(sessionBeads.Open()),
		})
	}
	if cycle != nil {
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

func (c *SessionReconcilerTraceCycle) recordDecision(siteCode, template, sessionName, reason, outcome string, data traceRecordPayload, _ []string, _ string) {
	if c == nil {
		return
	}
	fields := make(map[string]any, len(data))
	for k, v := range data {
		fields[k] = v
	}
	normSite, rawSite := normalizeTraceSiteCode(siteCode)
	normReason, rawReason := normalizeTraceReasonCode(reason)
	normOutcome, rawOutcome := normalizeTraceOutcomeCode(outcome)
	if rawSite != "" {
		fields["raw_site_code"] = rawSite
	}
	if rawReason != "" {
		fields["raw_reason_code"] = rawReason
	}
	if rawOutcome != "" {
		fields["raw_outcome_code"] = rawOutcome
	}
	c.RecordDecision(normSite, normReason, normOutcome, template, sessionName, fields)
}

func (c *SessionReconcilerTraceCycle) recordOperation(siteCode, template, sessionName, _ string, reason, outcome string, data traceRecordPayload, _ string) {
	if c == nil {
		return
	}
	fields := make(map[string]any, len(data)+1)
	for k, v := range data {
		fields[k] = v
	}
	var duration time.Duration
	if durMs, ok := fields["duration_ms"].(int64); ok {
		duration = time.Duration(durMs) * time.Millisecond
	}
	normSite, rawSite := normalizeTraceSiteCode(siteCode)
	normReason, rawReason := normalizeTraceReasonCode(reason)
	normOutcome, rawOutcome := normalizeTraceOutcomeCode(outcome)
	if rawSite != "" {
		fields["raw_site_code"] = rawSite
	}
	if rawReason != "" {
		fields["raw_reason_code"] = rawReason
	}
	if rawOutcome != "" {
		fields["raw_outcome_code"] = rawOutcome
	}
	c.RecordOperation(normSite, normReason, normOutcome, "", template, sessionName, duration, fields)
}

func (c *SessionReconcilerTraceCycle) recordMutation(siteCode, template, _ string, targetKind, targetID, writeMethod string, before, after any, outcome string, data traceRecordPayload, _ string) {
	if c == nil {
		return
	}
	fields := make(map[string]any, len(data)+3)
	for k, v := range data {
		fields[k] = v
	}
	fields["template"] = template
	fields["before"] = before
	fields["after"] = after
	fields["field"] = writeMethod
	normSite, rawSite := normalizeTraceSiteCode(siteCode)
	normOutcome, rawOutcome := normalizeTraceOutcomeCode(outcome)
	if rawSite != "" {
		fields["raw_site_code"] = rawSite
	}
	if rawOutcome != "" {
		fields["raw_outcome_code"] = rawOutcome
	}
	c.RecordMutation(normSite, TraceReasonUnknown, normOutcome, targetKind, targetID, writeMethod, fields)
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
