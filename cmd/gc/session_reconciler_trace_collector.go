package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

const (
	sessionReconcilerTraceMaxRecordsPerCycle = 4000
	sessionReconcilerTraceMaxDerivedDeps     = 4
	sessionReconcilerTraceMaxAutoArms        = 4
	sessionReconcilerTracePendingRecordsCap  = 400
	sessionReconcilerTraceMetadataWait       = 10 * time.Millisecond
	sessionReconcilerTraceDurableWait        = 25 * time.Millisecond
	sessionReconcilerTraceFlushQueueSize     = 8
)

var (
	errTraceFlushQueueFull          = errors.New("trace flush queue full")
	errTraceFlushWaitBudgetExceeded = errors.New("trace flush wait budget exceeded")
)

type sessionReconcilerTraceFlushRequest struct {
	records    []SessionReconcilerTraceRecord
	durability TraceDurabilityTier
	result     chan error
}

type SessionReconcilerTracer struct {
	mu              sync.Mutex
	cityPath        string
	cityName        string
	version         string
	commit          string
	date            string
	host            string
	pid             int
	startedAt       time.Time
	enabled         bool
	stderr          io.Writer
	store           *SessionReconcilerTraceStore
	armStore        *SessionReconcilerTraceArmStore
	lastArms        map[string]TraceArm
	detail          map[string]TraceSource
	cycleCount      uint64
	flushCh         chan sessionReconcilerTraceFlushRequest
	flushDone       chan struct{}
	closeCh         chan struct{}
	closing         bool
	autoArmInflight int
	enqueueWG       sync.WaitGroup
}

type SessionReconcilerTraceCycle struct {
	mu                 sync.Mutex
	tracer             *SessionReconcilerTracer
	cfg                *config.City
	configRevision     string
	traceID            string
	tickID             string
	start              time.Time
	trigger            TraceTickTrigger
	triggerDetail      string
	records            []SessionReconcilerTraceRecord
	recordCount        int
	droppedRecords     int
	droppedBatches     int
	dropReasons        map[string]int
	completionStatus   TraceCompletionStatus
	traceMode          TraceMode
	traceSource        TraceSource
	controllerInstance string
	controllerStarted  time.Time
	pendingDetail      map[string][]SessionReconcilerTraceRecord
	pendingDropped     map[string]int
	templatesTouched   map[string]struct{}
	detailedTemplates  map[string]struct{}
	decisionCounts     map[string]int
	operationCounts    map[string]int
	mutationCounts     map[string]int
	reasonCounts       map[string]int
	outcomeCounts      map[string]int
	autoArmsTriggered  int
}

func newSessionReconcilerTracer(cityPath, cityName string, stderr io.Writer) *SessionReconcilerTracer {
	if strings.TrimSpace(os.Getenv("GC_SESSION_RECONCILER_TRACE")) == "0" {
		return &SessionReconcilerTracer{cityPath: cityPath, cityName: cityName, stderr: stderr, enabled: false}
	}
	host, _ := os.Hostname()
	store, err := newSessionReconcilerTraceStore(cityPath, stderr)
	if err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "trace: disabled: %v\n", err) //nolint:errcheck
		}
		return &SessionReconcilerTracer{cityPath: cityPath, cityName: cityName, stderr: stderr, enabled: false}
	}
	tracer := &SessionReconcilerTracer{
		cityPath:  cityPath,
		cityName:  cityName,
		version:   version,
		commit:    commit,
		date:      date,
		host:      host,
		pid:       os.Getpid(),
		startedAt: time.Now().UTC(),
		enabled:   true,
		stderr:    stderr,
		store:     store,
		armStore:  newSessionReconcilerTraceArmStore(cityPath),
		lastArms:  make(map[string]TraceArm),
		flushCh:   make(chan sessionReconcilerTraceFlushRequest, sessionReconcilerTraceFlushQueueSize),
		flushDone: make(chan struct{}),
		closeCh:   make(chan struct{}),
	}
	go tracer.runFlushLoop()
	return tracer
}

func (t *SessionReconcilerTracer) Enabled() bool {
	return t != nil && t.enabled && t.store != nil
}

func (t *SessionReconcilerTracer) Close() error {
	if t == nil || t.store == nil {
		return nil
	}
	t.mu.Lock()
	if t.closing {
		flushDone := t.flushDone
		t.mu.Unlock()
		if flushDone != nil {
			<-flushDone
		}
		return nil
	}
	t.closing = true
	flushCh := t.flushCh
	flushDone := t.flushDone
	closeCh := t.closeCh
	t.flushCh = nil
	t.mu.Unlock()
	if closeCh != nil {
		close(closeCh)
	}
	t.enqueueWG.Wait()
	if flushCh != nil {
		close(flushCh)
	}
	if flushDone != nil {
		<-flushDone
	}
	return t.store.Close()
}

func (t *SessionReconcilerTracer) runFlushLoop() {
	defer close(t.flushDone)
	for req := range t.flushCh {
		err := t.store.AppendBatch(req.records, req.durability)
		select {
		case req.result <- err:
		default:
		}
	}
}

func (t *SessionReconcilerTracer) appendBatch(records []SessionReconcilerTraceRecord, durability TraceDurabilityTier, waitBudget time.Duration) error {
	if t == nil || t.store == nil || len(records) == 0 {
		return nil
	}
	t.mu.Lock()
	if t.closing {
		t.mu.Unlock()
		return nil
	}
	flushCh := t.flushCh
	closeCh := t.closeCh
	if flushCh == nil {
		t.mu.Unlock()
		return t.store.AppendBatch(records, durability)
	}
	t.enqueueWG.Add(1)
	t.mu.Unlock()
	defer t.enqueueWG.Done()
	start := time.Now()
	resultCh := make(chan error, 1)
	req := sessionReconcilerTraceFlushRequest{
		records:    records,
		durability: durability,
		result:     resultCh,
	}
	timer := time.NewTimer(waitBudget)
	select {
	case <-closeCh:
		timer.Stop()
		return nil
	case flushCh <- req:
	case <-timer.C:
		timer.Stop()
		return errTraceFlushQueueFull
	}
	timer.Stop()
	remaining := waitBudget - time.Since(start)
	if remaining <= 0 {
		return errTraceFlushWaitBudgetExceeded
	}
	timer = time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case err := <-resultCh:
		return err
	case <-timer.C:
		return errTraceFlushWaitBudgetExceeded
	}
}

func (t *SessionReconcilerTracer) BeginCycle(trigger TraceTickTrigger, triggerDetail string, now time.Time, cfg *config.City) *SessionReconcilerTraceCycle {
	if !t.Enabled() {
		return nil
	}
	t.mu.Lock()
	t.cycleCount++
	cycleNum := t.cycleCount
	t.mu.Unlock()
	traceID := newTraceID("cycle")
	tickID := fmt.Sprintf("%s-%d-%d-%06d", t.cityName, t.pid, t.startedAt.UnixNano(), cycleNum)
	cycle := &SessionReconcilerTraceCycle{
		tracer:             t,
		cfg:                cfg,
		traceID:            traceID,
		tickID:             tickID,
		start:              now.UTC(),
		trigger:            trigger,
		triggerDetail:      triggerDetail,
		records:            nil,
		dropReasons:        make(map[string]int),
		completionStatus:   TraceCompletionCompleted,
		traceMode:          TraceModeBaseline,
		traceSource:        TraceSourceAlwaysOn,
		controllerInstance: fmt.Sprintf("%s:%d", t.host, t.pid),
		controllerStarted:  t.startedAt,
		pendingDetail:      make(map[string][]SessionReconcilerTraceRecord),
		pendingDropped:     make(map[string]int),
		templatesTouched:   make(map[string]struct{}),
		detailedTemplates:  make(map[string]struct{}),
		decisionCounts:     make(map[string]int),
		operationCounts:    make(map[string]int),
		mutationCounts:     make(map[string]int),
		reasonCounts:       make(map[string]int),
		outcomeCounts:      make(map[string]int),
	}
	cycle.addRecord(newTraceRecord(TraceRecordCycleStart).withCycle(cycle, now.UTC()).withTrigger(trigger, triggerDetail))
	cycle.syncArms(now.UTC(), cfg)
	return cycle
}

func (c *SessionReconcilerTraceCycle) addRecord(rec SessionReconcilerTraceRecord) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.records) >= sessionReconcilerTraceMaxRecordsPerCycle {
		c.droppedRecords++
		c.dropReasons["record_budget_exceeded"]++
		return
	}
	c.accumulateRecordLocked(rec)
	c.records = append(c.records, rec)
	c.recordCount++
}

func (c *SessionReconcilerTraceCycle) accumulateRecordLocked(rec SessionReconcilerTraceRecord) {
	if rec.Template != "" {
		c.templatesTouched[rec.Template] = struct{}{}
		if rec.TraceMode == TraceModeDetail {
			c.detailedTemplates[rec.Template] = struct{}{}
		}
	}
	switch rec.RecordType {
	case TraceRecordDecision:
		c.decisionCounts[string(rec.SiteCode)]++
	case TraceRecordOperation:
		c.operationCounts[string(rec.SiteCode)]++
	case TraceRecordMutation:
		c.mutationCounts[string(rec.SiteCode)]++
	case TraceRecordTraceControl:
		action, _ := rec.Fields["action"].(string)
		source, _ := rec.Fields["source"].(TraceArmSource)
		if source == "" {
			if raw, ok := rec.Fields["source"].(string); ok {
				source = TraceArmSource(raw)
			}
		}
		if action == "start" && source == TraceArmSourceAuto {
			c.autoArmsTriggered++
		}
	}
	if rec.ReasonCode != "" {
		c.reasonCounts[string(rec.ReasonCode)]++
	}
	if rec.OutcomeCode != "" {
		c.outcomeCounts[string(rec.OutcomeCode)]++
	}
}

func (c *SessionReconcilerTraceCycle) stashPendingDetail(template string, rec SessionReconcilerTraceRecord) {
	if c == nil {
		return
	}
	template = normalizedTraceTemplate(template)
	if template == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	queue := c.pendingDetail[template]
	if len(queue) >= sessionReconcilerTracePendingRecordsCap {
		queue = append(queue[1:], rec)
		c.pendingDropped[template]++
		c.pendingDetail[template] = queue
		return
	}
	c.pendingDetail[template] = append(queue, rec)
}

func (c *SessionReconcilerTraceCycle) promotePendingDetail(template string) {
	if c == nil {
		return
	}
	template = normalizedTraceTemplate(template)
	if template == "" {
		return
	}
	c.mu.Lock()
	pending := append([]SessionReconcilerTraceRecord(nil), c.pendingDetail[template]...)
	dropped := c.pendingDropped[template]
	delete(c.pendingDetail, template)
	delete(c.pendingDropped, template)
	c.mu.Unlock()
	if len(pending) == 0 && dropped == 0 {
		return
	}
	source := TraceSource(c.sourceFor(template))
	for i := range pending {
		pending[i].TraceMode = TraceModeDetail
		pending[i].TraceSource = source
	}
	for _, rec := range pending {
		c.addRecord(rec)
	}
	if dropped > 0 {
		c.RecordTraceControl("promote", TraceArmScopeTemplate, template, traceArmSourceFromTraceSource(source), TraceReasonRetained, TraceOutcomePromotionPartialContext, map[string]any{
			"dropped_pending_records": dropped,
		})
	}
}

func traceArmSourceFromTraceSource(source TraceSource) TraceArmSource {
	if source == TraceSourceAuto {
		return TraceArmSourceAuto
	}
	return TraceArmSourceManual
}

func (r SessionReconcilerTraceRecord) withCycle(c *SessionReconcilerTraceCycle, now time.Time) SessionReconcilerTraceRecord {
	if c == nil {
		return r
	}
	r.TraceID = c.traceID
	r.TickID = c.tickID
	r.Ts = now.UTC()
	r.CycleOffsetMS = now.UTC().Sub(c.start).Milliseconds()
	r.CityPath = c.tracer.cityPath
	r.ConfigRevision = c.configRevision
	r.GCVersion = c.tracer.version
	r.GCCommit = c.tracer.commit
	r.BuildDate = c.tracer.date
	r.ControllerInstanceID = c.controllerInstance
	r.ControllerPID = c.tracer.pid
	r.ControllerStartedAt = &c.controllerStarted
	r.Host = c.tracer.host
	r.TraceMode = c.traceMode
	r.TraceSource = c.traceSource
	return r
}

func (r SessionReconcilerTraceRecord) withTrigger(trigger TraceTickTrigger, detail string) SessionReconcilerTraceRecord {
	r.TickTrigger = trigger
	r.TriggerDetail = detail
	return r
}

func (c *SessionReconcilerTraceCycle) detailSource(template string) (string, bool) {
	if c == nil || c.tracer == nil {
		return "", false
	}
	key := normalizedTraceTemplate(template)
	if key == "" {
		return "", false
	}
	c.tracer.mu.Lock()
	detail := c.tracer.detail
	c.tracer.mu.Unlock()
	if detail == nil {
		return "", false
	}
	source, ok := detail[key]
	if !ok {
		return "", false
	}
	return string(source), true
}

func (c *SessionReconcilerTraceCycle) record(kind TraceRecordType, site TraceSiteCode, reason TraceReasonCode, outcome TraceOutcomeCode, now time.Time, fields map[string]any) {
	if c == nil {
		return
	}
	rec := newTraceRecord(kind).withCycle(c, now)
	rec.SiteCode = site
	rec.ReasonCode = reason
	rec.OutcomeCode = outcome
	if len(fields) > 0 {
		rec.ensureFields()
		for k, v := range fields {
			rec.Fields[k] = v
		}
	}
	c.addRecord(rec)
}

func (c *SessionReconcilerTraceCycle) syncArms(now time.Time, cfg *config.City) {
	if c == nil || c.tracer == nil || c.tracer.armStore == nil {
		return
	}
	c.mu.Lock()
	c.cfg = cfg
	c.mu.Unlock()
	state, err := c.tracer.armStore.list()
	if err != nil {
		c.record(TraceRecordTraceControl, TraceSiteTraceControl, TraceReasonUnknown, TraceOutcomeFailed, now, map[string]any{
			"action": "status",
			"error":  err.Error(),
		})
		return
	}
	arms := traceArmStatus(state, now)
	detail := buildTraceDetailScopes(cfg, arms)
	current := make(map[string]TraceArm, len(arms))
	for _, arm := range arms {
		current[traceScopeKey(arm.ScopeType, arm.ScopeValue, arm.Source)] = arm
	}
	c.tracer.mu.Lock()
	last := c.tracer.lastArms
	c.tracer.mu.Unlock()
	for key, arm := range current {
		if prev, ok := last[key]; ok {
			if !prev.ExpiresAt.Equal(arm.ExpiresAt) || !prev.LastExtendedAt.Equal(arm.LastExtendedAt) {
				c.record(TraceRecordTraceControl, TraceSiteTraceControl, TraceReasonRetained, TraceOutcomeApplied, now, map[string]any{
					"action":           "extend",
					"scope_type":       arm.ScopeType,
					"scope_value":      arm.ScopeValue,
					"source":           arm.Source,
					"expires_at":       arm.ExpiresAt,
					"last_extended_at": arm.LastExtendedAt,
				})
			}
			continue
		}
		c.record(TraceRecordTraceControl, TraceSiteTraceControl, TraceReasonRetained, TraceOutcomeApplied, now, map[string]any{
			"action":      "start",
			"scope_type":  arm.ScopeType,
			"scope_value": arm.ScopeValue,
			"source":      arm.Source,
			"level":       arm.Level,
			"expires_at":  arm.ExpiresAt,
			"armed_at":    arm.ArmedAt,
		})
	}
	for key, arm := range last {
		if _, ok := current[key]; ok {
			continue
		}
		action := "stop"
		reason := TraceReasonExpired
		if !arm.ExpiresAt.IsZero() && arm.ExpiresAt.Before(now) {
			action = "expire"
		}
		c.record(TraceRecordTraceControl, TraceSiteTraceControl, reason, TraceOutcomeApplied, now, map[string]any{
			"action":      action,
			"scope_type":  arm.ScopeType,
			"scope_value": arm.ScopeValue,
			"source":      arm.Source,
			"level":       arm.Level,
		})
	}
	c.tracer.mu.Lock()
	c.tracer.detail = detail
	c.tracer.lastArms = current
	c.tracer.mu.Unlock()
}

func buildTraceDetailScopes(cfg *config.City, arms []TraceArm) map[string]TraceSource {
	if len(arms) == 0 {
		return nil
	}
	scopes := make(map[string]TraceSource)
	for _, arm := range arms {
		if arm.ScopeType != TraceArmScopeTemplate {
			continue
		}
		if arm.Level != TraceModeDetail {
			continue
		}
		template := normalizedTraceTemplate(arm.ScopeValue)
		if template == "" {
			continue
		}
		scopes[template] = TraceSource(arm.Source)
		if cfg == nil {
			continue
		}
		agent := findAgentByTemplate(cfg, template)
		if agent == nil {
			continue
		}
		for idx, dep := range agent.DependsOn {
			if idx >= sessionReconcilerTraceMaxDerivedDeps {
				break
			}
			depTemplate := normalizedTraceTemplate(dep)
			if depTemplate == "" {
				continue
			}
			if _, exists := scopes[depTemplate]; exists {
				continue
			}
			scopes[depTemplate] = TraceSourceDerivedDependency
		}
	}
	if len(scopes) == 0 {
		return nil
	}
	return scopes
}

func (c *SessionReconcilerTraceCycle) RecordConfigReload(previousRev, newRev string, outcome TraceOutcomeCode, added, removed []string, providerChanged bool, err error) {
	if c == nil {
		return
	}
	fields := map[string]any{
		"previous_config_revision": previousRev,
		"new_config_revision":      newRev,
		"added_templates":          added,
		"removed_templates":        removed,
		"provider_changed":         providerChanged,
	}
	if err != nil {
		fields["error"] = err.Error()
	}
	c.record(TraceRecordConfigReload, TraceSiteConfigReload, TraceReasonUnknown, outcome, time.Now().UTC(), fields)
}

func (c *SessionReconcilerTraceCycle) RecordCycleInputSnapshot(summary map[string]any) {
	if c == nil {
		return
	}
	c.record(TraceRecordCycleInputSnapshot, TraceSiteDesiredStateBuild, TraceReasonRetained, TraceOutcomeApplied, time.Now().UTC(), summary)
}

func (c *SessionReconcilerTraceCycle) RecordTemplateSummary(template string, sessionName string, status TraceEvaluationStatus, reason TraceReasonCode, fields map[string]any) {
	if c == nil {
		return
	}
	rec := newTraceRecord(TraceRecordTemplateTickSummary).withCycle(c, time.Now().UTC())
	rec.Template = template
	rec.SessionName = sessionName
	rec.EvaluationStatus = status
	rec.ReasonCode = reason
	if len(fields) > 0 {
		rec.ensureFields()
		for k, v := range fields {
			rec.Fields[k] = v
		}
	}
	c.addRecord(rec)
}

func (c *SessionReconcilerTraceCycle) RecordTemplateConfigSnapshot(template string, fields map[string]any) {
	if c == nil {
		return
	}
	rec := newTraceRecord(TraceRecordTemplateConfig).withCycle(c, time.Now().UTC())
	rec.Template = template
	rec.TraceMode = TraceModeDetail
	rec.TraceSource = TraceSource(c.sourceFor(template))
	if len(fields) > 0 {
		rec.ensureFields()
		for k, v := range fields {
			rec.Fields[k] = v
		}
	}
	if _, ok := c.detailSource(template); !ok {
		c.stashPendingDetail(template, rec)
		return
	}
	c.addRecord(rec)
}

func (c *SessionReconcilerTraceCycle) RecordSessionBaseline(template, sessionName string, fields map[string]any) {
	if c == nil {
		return
	}
	rec := newTraceRecord(TraceRecordSessionBaseline).withCycle(c, time.Now().UTC())
	rec.Template = template
	rec.SessionName = sessionName
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

func (c *SessionReconcilerTraceCycle) RecordSessionResult(template, sessionName string, outcome TraceOutcomeCode, completeness TraceCompletenessStatus, fields map[string]any) {
	if c == nil {
		return
	}
	rec := newTraceRecord(TraceRecordSessionResult).withCycle(c, time.Now().UTC())
	rec.Template = template
	rec.SessionName = sessionName
	rec.OutcomeCode = outcome
	rec.CompletenessStatus = completeness
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

func (c *SessionReconcilerTraceCycle) RecordDecision(site TraceSiteCode, reason TraceReasonCode, outcome TraceOutcomeCode, template, sessionName string, fields map[string]any) {
	if c == nil {
		return
	}
	rec := newTraceRecord(TraceRecordDecision).withCycle(c, time.Now().UTC())
	rec.SiteCode = site
	rec.ReasonCode = reason
	rec.OutcomeCode = outcome
	rec.Template = template
	rec.SessionName = sessionName
	rec.TraceMode = TraceModeDetail
	rec.TraceSource = TraceSource(c.sourceFor(template))
	if len(fields) > 0 {
		rec.ensureFields()
		for k, v := range fields {
			rec.Fields[k] = v
		}
	}
	if _, ok := c.detailSource(template); !ok {
		if c.ensureAutoArm(template, reason, outcome) {
			rec.TraceSource = TraceSource(c.sourceFor(template))
		} else {
			c.stashPendingDetail(template, rec)
			return
		}
	}
	c.addRecord(rec)
}

func (c *SessionReconcilerTraceCycle) RecordOperation(site TraceSiteCode, reason TraceReasonCode, outcome TraceOutcomeCode, opName, template, sessionName string, duration time.Duration, fields map[string]any) {
	if c == nil {
		return
	}
	rec := newTraceRecord(TraceRecordOperation).withCycle(c, time.Now().UTC())
	rec.SiteCode = site
	rec.ReasonCode = reason
	rec.OutcomeCode = outcome
	rec.Template = template
	rec.SessionName = sessionName
	rec.OperationID = newTraceID(opName)
	rec.TraceMode = TraceModeDetail
	rec.TraceSource = TraceSource(c.sourceFor(template))
	rec.DurationMS = duration.Milliseconds()
	if len(fields) > 0 {
		rec.ensureFields()
		for k, v := range fields {
			rec.Fields[k] = v
		}
	}
	if _, ok := c.detailSource(template); !ok {
		if c.ensureAutoArm(template, reason, outcome) {
			rec.TraceSource = TraceSource(c.sourceFor(template))
		} else {
			c.stashPendingDetail(template, rec)
			return
		}
	}
	c.addRecord(rec)
}

func (c *SessionReconcilerTraceCycle) RecordMutation(site TraceSiteCode, reason TraceReasonCode, outcome TraceOutcomeCode, targetKind, targetID, writeMethod string, fields map[string]any) {
	if c == nil {
		return
	}
	template, _ := fields["template"].(string)
	rec := newTraceRecord(TraceRecordMutation).withCycle(c, time.Now().UTC())
	rec.SiteCode = site
	rec.ReasonCode = reason
	rec.OutcomeCode = outcome
	rec.TraceMode = TraceModeDetail
	rec.TraceSource = TraceSource(c.sourceFor(template))
	rec.ensureFields()
	for k, v := range fields {
		rec.Fields[k] = v
	}
	rec.Fields["target_kind"] = targetKind
	rec.Fields["target_id"] = targetID
	rec.Fields["write_method"] = writeMethod
	if template != "" {
		if _, ok := c.detailSource(template); !ok {
			if c.ensureAutoArm(template, reason, outcome) {
				rec.TraceSource = TraceSource(c.sourceFor(template))
			} else {
				c.stashPendingDetail(template, rec)
				return
			}
		}
	}
	c.addRecord(rec)
}

func (c *SessionReconcilerTraceCycle) ensureAutoArm(template string, reason TraceReasonCode, outcome TraceOutcomeCode) bool {
	if c == nil || c.tracer == nil || c.tracer.armStore == nil {
		return false
	}
	template = normalizedTraceTemplate(template)
	if template == "" {
		return false
	}
	if _, ok := c.detailSource(template); ok {
		return true
	}
	if !shouldAutoArmForTrace(reason, outcome) {
		return false
	}
	c.tracer.mu.Lock()
	activeAutoArms := 0
	for _, arm := range c.tracer.lastArms {
		if arm.Source == TraceArmSourceAuto {
			activeAutoArms++
		}
	}
	if activeAutoArms+c.tracer.autoArmInflight >= sessionReconcilerTraceMaxAutoArms {
		c.tracer.mu.Unlock()
		c.RecordTraceControl("suppress", TraceArmScopeTemplate, template, TraceArmSourceAuto, TraceReasonAutoArmSuppressed, TraceOutcomeNoChange, map[string]any{
			"active_auto_arms":   activeAutoArms,
			"inflight_auto_arms": c.tracer.autoArmInflight,
		})
		return false
	}
	c.tracer.autoArmInflight++
	c.tracer.mu.Unlock()
	defer func() {
		c.tracer.mu.Lock()
		if c.tracer.autoArmInflight > 0 {
			c.tracer.autoArmInflight--
		}
		c.tracer.mu.Unlock()
	}()
	now := time.Now().UTC()
	arm := TraceArm{
		ScopeType:      TraceArmScopeTemplate,
		ScopeValue:     template,
		Source:         TraceArmSourceAuto,
		Level:          TraceModeDetail,
		ArmedAt:        now,
		ExpiresAt:      now.Add(10 * time.Minute),
		LastExtendedAt: now,
		TriggerReason:  string(reason),
		UpdatedAt:      now,
	}
	state, err := c.tracer.armStore.upsertArm(arm)
	if err != nil {
		c.mu.Lock()
		c.droppedRecords++
		c.dropReasons["auto_arm_failed"]++
		c.mu.Unlock()
		return false
	}
	c.mu.Lock()
	cfg := c.cfg
	c.mu.Unlock()
	active := traceArmStatus(state, now)
	scopes := buildTraceDetailScopes(cfg, active)
	c.tracer.mu.Lock()
	c.tracer.detail = scopes
	if c.tracer.lastArms == nil {
		c.tracer.lastArms = make(map[string]TraceArm)
	}
	c.tracer.lastArms[traceScopeKey(arm.ScopeType, arm.ScopeValue, arm.Source)] = arm
	c.tracer.mu.Unlock()
	c.RecordTraceControl("start", TraceArmScopeTemplate, template, TraceArmSourceAuto, reason, TraceOutcomeApplied, map[string]any{
		"level":          TraceModeDetail,
		"trigger_reason": string(reason),
		"expires_at":     arm.ExpiresAt,
	})
	c.promotePendingDetail(template)
	_ = c.flushCurrentBatch(TraceDurabilityDurable)
	return true
}

func shouldAutoArmForTrace(reason TraceReasonCode, outcome TraceOutcomeCode) bool {
	switch reason {
	case TraceReasonPendingCreateRollback,
		TraceReasonWakeFailureIncremented,
		TraceReasonQuarantineEntered,
		TraceReasonStorePartial,
		TraceReasonConfigDrift,
		TraceReasonUnknownStateSkipped:
		return true
	}
	switch outcome {
	case TraceOutcomeFailed, TraceOutcomeProviderError, TraceOutcomeDeadlineExceeded:
		return true
	}
	return false
}

func (c *SessionReconcilerTraceCycle) RecordTraceControl(action string, scopeType TraceArmScopeType, scopeValue string, source TraceArmSource, reason TraceReasonCode, outcome TraceOutcomeCode, fields map[string]any) {
	if c == nil {
		return
	}
	rec := newTraceRecord(TraceRecordTraceControl).withCycle(c, time.Now().UTC())
	rec.SiteCode = TraceSiteTraceControl
	rec.ReasonCode = reason
	rec.OutcomeCode = outcome
	rec.TraceMode = TraceModeBaseline
	rec.TraceSource = TraceSourceAlwaysOn
	rec.ensureFields()
	for k, v := range fields {
		rec.Fields[k] = v
	}
	rec.Fields["action"] = action
	rec.Fields["scope_type"] = scopeType
	rec.Fields["scope_value"] = scopeValue
	rec.Fields["source"] = source
	c.addRecord(rec)
}

func (c *SessionReconcilerTraceCycle) End(completion TraceCompletionStatus, fields map[string]any) error {
	if c == nil || c.tracer == nil || !c.tracer.Enabled() {
		return nil
	}
	now := time.Now().UTC()
	dur := now.Sub(c.start)
	c.mu.Lock()
	batch := append([]SessionReconcilerTraceRecord(nil), c.records...)
	droppedRecords := c.droppedRecords
	droppedBatches := c.droppedBatches
	dropReasons := make(map[string]int, len(c.dropReasons))
	for k, v := range c.dropReasons {
		dropReasons[k] = v
	}
	templatesTouched := traceSetStrings(c.templatesTouched)
	detailedTemplateCount := len(c.detailedTemplates)
	decisionCounts := make(map[string]int, len(c.decisionCounts))
	for k, v := range c.decisionCounts {
		decisionCounts[k] = v
	}
	operationCounts := make(map[string]int, len(c.operationCounts))
	for k, v := range c.operationCounts {
		operationCounts[k] = v
	}
	mutationCounts := make(map[string]int, len(c.mutationCounts))
	for k, v := range c.mutationCounts {
		mutationCounts[k] = v
	}
	reasonCounts := make(map[string]int, len(c.reasonCounts))
	for k, v := range c.reasonCounts {
		reasonCounts[k] = v
	}
	outcomeCounts := make(map[string]int, len(c.outcomeCounts))
	for k, v := range c.outcomeCounts {
		outcomeCounts[k] = v
	}
	autoArmsTriggered := c.autoArmsTriggered
	c.mu.Unlock()
	rollup := map[string]any{
		"active_template_count":   coalesceTraceField(fields["active_template_count"], len(templatesTouched)),
		"detailed_template_count": coalesceTraceField(fields["detailed_template_count"], detailedTemplateCount),
		"templates_touched":       coalesceTraceField(fields["templates_touched"], templatesTouched),
		"decision_counts":         coalesceTraceField(fields["decision_counts"], decisionCounts),
		"operation_counts":        coalesceTraceField(fields["operation_counts"], operationCounts),
		"mutation_counts":         coalesceTraceField(fields["mutation_counts"], mutationCounts),
		"reason_counts":           coalesceTraceField(fields["reason_counts"], reasonCounts),
		"outcome_counts":          coalesceTraceField(fields["outcome_counts"], outcomeCounts),
		"auto_arms_triggered":     coalesceTraceField(fields["auto_arms_triggered"], autoArmsTriggered),
		"dropped_record_count":    droppedRecords,
		"dropped_batch_count":     droppedBatches,
		"drop_reason_counts":      dropReasons,
	}
	rec := newTraceRecord(TraceRecordCycleResult).withCycle(c, now)
	rec.SiteCode = TraceSiteCycleFinish
	rec.CompletionStatus = completion
	rec.DurationMS = dur.Milliseconds()
	rec.RecordCount = c.recordCount + 1
	rec.SeqStart = 0
	rec.SeqEnd = 0
	rec.DroppedRecordCount = droppedRecords
	rec.DroppedBatchCount = droppedBatches
	rec.DropReasonCounts = dropReasons
	rec.TraceMode = TraceModeBaseline
	rec.TraceSource = TraceSourceAlwaysOn
	rec.ensureFields()
	for k, v := range rollup {
		if v != nil {
			rec.Fields[k] = v
		}
	}
	batch = append(batch, rec)
	if c.tracer.store == nil {
		return nil
	}
	sortTraceRecords(batch)
	if err := c.tracer.appendBatch(batch, TraceDurabilityDurable, sessionReconcilerTraceDurableWait); err != nil {
		if errors.Is(err, errTraceFlushQueueFull) {
			c.addDropped("flush_queue_full", len(batch))
			c.mu.Lock()
			c.droppedBatches++
			c.mu.Unlock()
			if c.tracer.stderr != nil {
				fmt.Fprintf(c.tracer.stderr, "trace: flush_queue_full: %s %s\n", c.tickID, TraceDurabilityDurable) //nolint:errcheck
			}
			return nil
		}
		if errors.Is(err, errTraceFlushWaitBudgetExceeded) {
			if c.tracer.stderr != nil {
				fmt.Fprintf(c.tracer.stderr, "trace: slow_storage_degraded: %s %s\n", c.tickID, TraceDurabilityDurable) //nolint:errcheck
			}
			return nil
		}
		if c.tracer.stderr != nil {
			fmt.Fprintf(c.tracer.stderr, "trace: append: %v\n", err) //nolint:errcheck
		}
		c.mu.Lock()
		c.droppedBatches++
		c.droppedRecords += len(batch)
		c.mu.Unlock()
		return err
	}
	return nil
}

func (c *SessionReconcilerTraceCycle) flushCurrentBatch(durability TraceDurabilityTier) error {
	if c == nil || c.tracer == nil {
		return nil
	}
	c.mu.Lock()
	if len(c.records) == 0 {
		c.mu.Unlock()
		return nil
	}
	batch := append([]SessionReconcilerTraceRecord(nil), c.records...)
	c.records = nil
	c.mu.Unlock()
	sortTraceRecords(batch)
	if err := c.tracer.appendBatch(batch, durability, traceFlushBudget(durability)); err != nil {
		if errors.Is(err, errTraceFlushQueueFull) {
			c.addDropped("flush_queue_full", len(batch))
			c.mu.Lock()
			c.droppedBatches++
			c.mu.Unlock()
			if c.tracer.stderr != nil {
				fmt.Fprintf(c.tracer.stderr, "trace: flush_queue_full: %s %s\n", c.tickID, durability) //nolint:errcheck
			}
			return nil
		}
		if errors.Is(err, errTraceFlushWaitBudgetExceeded) {
			if c.tracer.stderr != nil {
				fmt.Fprintf(c.tracer.stderr, "trace: slow_storage_degraded: %s %s\n", c.tickID, durability) //nolint:errcheck
			}
			return nil
		}
		c.addDropped("flush_failed", len(batch))
		if c.tracer.stderr != nil {
			fmt.Fprintf(c.tracer.stderr, "trace: append: %v\n", err) //nolint:errcheck
		}
		return err
	}
	return nil
}

func traceFlushBudget(durability TraceDurabilityTier) time.Duration {
	if durability == TraceDurabilityMetadata {
		return sessionReconcilerTraceMetadataWait
	}
	return sessionReconcilerTraceDurableWait
}

func (c *SessionReconcilerTraceCycle) addDropped(reason string, n int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dropReasons == nil {
		c.dropReasons = make(map[string]int)
	}
	c.dropReasons[reason] += n
	c.droppedRecords += n
}

func coalesceTraceField(primary, fallback any) any {
	if primary != nil {
		return primary
	}
	return fallback
}

func traceSetStrings(values map[string]struct{}) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
