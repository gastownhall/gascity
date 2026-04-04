package main

import (
	"fmt"
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

func (c *SessionReconcilerTraceCycle) recordDecision(siteCode, template, sessionName, reason, outcome string, data traceRecordPayload, _ []string, _ string) string {
	if c == nil {
		return ""
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
	return ""
}

func (c *SessionReconcilerTraceCycle) recordOperation(siteCode, template, sessionName, operationID, reason, outcome string, data traceRecordPayload, _ string) string {
	if c == nil {
		return ""
	}
	fields := make(map[string]any, len(data)+1)
	for k, v := range data {
		fields[k] = v
	}
	if operationID != "" {
		fields["operation_id"] = operationID
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
	c.RecordOperation(normSite, normReason, normOutcome, operationID, template, sessionName, duration, fields)
	return ""
}

func (c *SessionReconcilerTraceCycle) recordMutation(siteCode, template, sessionName, targetKind, targetID, writeMethod string, before, after any, outcome string, data traceRecordPayload, _ string) string {
	if c == nil {
		return ""
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
	return ""
}

func (c *SessionReconcilerTraceCycle) recordTemplateSummary(template, sessionName, siteCode, reason, outcome string, data traceRecordPayload) string {
	if c == nil {
		return ""
	}
	fields := make(map[string]any, len(data))
	for k, v := range data {
		fields[k] = v
	}
	status := TraceEvaluationStatus(outcome)
	normReason, rawReason := normalizeTraceReasonCode(reason)
	if rawReason != "" {
		fields["raw_reason_code"] = rawReason
	}
	c.RecordTemplateSummary(template, sessionName, status, normReason, fields)
	return ""
}

func (c *SessionReconcilerTraceCycle) recordTemplateConfigSnapshot(template, sessionName, siteCode string, data traceRecordPayload) string {
	if c == nil {
		return ""
	}
	fields := make(map[string]any, len(data))
	for k, v := range data {
		fields[k] = v
	}
	c.RecordTemplateConfigSnapshot(template, fields)
	return ""
}

func (c *SessionReconcilerTraceCycle) recordSessionBaseline(template, sessionName, siteCode string, data traceRecordPayload) string {
	if c == nil {
		return ""
	}
	fields := make(map[string]any, len(data))
	for k, v := range data {
		fields[k] = v
	}
	c.RecordSessionBaseline(template, sessionName, fields)
	return ""
}

func (c *SessionReconcilerTraceCycle) recordSessionResult(template, sessionName, siteCode, reason, outcome string, data traceRecordPayload) string {
	if c == nil {
		return ""
	}
	fields := make(map[string]any, len(data))
	for k, v := range data {
		fields[k] = v
	}
	c.RecordSessionResult(template, sessionName, TraceOutcomeCode(outcome), TraceCompletenessStatus(reason), fields)
	return ""
}

func (c *SessionReconcilerTraceCycle) recordConfigReload(outcome string, data traceRecordPayload) string {
	if c == nil {
		return ""
	}
	fields := make(map[string]any, len(data))
	for k, v := range data {
		fields[k] = v
	}
	previous, _ := fields["previous_config_revision"].(string)
	next, _ := fields["new_config_revision"].(string)
	if next == "" {
		next, _ = fields["revision"].(string)
	}
	added, _ := fields["added_templates"].([]string)
	removed, _ := fields["removed_templates"].([]string)
	providerChanged, _ := fields["provider_changed"].(bool)
	if !providerChanged {
		_, providerChanged = fields["provider"]
	}
	var err error
	if s, ok := fields["error"].(string); ok && s != "" {
		err = fmt.Errorf("%s", s)
	}
	c.RecordConfigReload(previous, next, TraceOutcomeCode(outcome), added, removed, providerChanged, err)
	return ""
}

func (c *SessionReconcilerTraceCycle) recordTraceControl(action, source, scopeType, scopeValue string, data traceRecordPayload) string {
	if c == nil {
		return ""
	}
	fields := make(map[string]any, len(data)+4)
	for k, v := range data {
		fields[k] = v
	}
	fields["action"] = action
	fields["source"] = source
	fields["scope_type"] = scopeType
	fields["scope_value"] = scopeValue
	c.RecordTraceControl(action, TraceArmScopeType(scopeType), scopeValue, TraceArmSource(source), TraceReasonRetained, TraceOutcomeApplied, fields)
	return ""
}

func (c *SessionReconcilerTraceCycle) flush(durability TraceDurabilityTier) error {
	if c == nil {
		return nil
	}
	return c.flushCurrentBatch(durability)
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
