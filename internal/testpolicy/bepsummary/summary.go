package bepsummary

import (
	"fmt"
	"io"
	"sort"
)

// ReportSchema is the version of the JSON report written by Run.
const ReportSchema = 1

// Outcome classifies how a test target's result was obtained.
type Outcome string

// Test target outcomes. A target is cached only when every run/shard result
// it reported was a cache hit; otherwise it executed.
const (
	// OutcomeCachedLocal: served from the client's local action cache
	// (bazel prints "(cached) PASSED"; BEP cachedLocally).
	OutcomeCachedLocal Outcome = "cached_local"
	// OutcomeCachedRemote: served from the remote cache (strategy
	// "remote cache hit").
	OutcomeCachedRemote Outcome = "cached_remote"
	// OutcomeCachedDisk: served from --disk_cache (strategy "disk cache hit").
	OutcomeCachedDisk Outcome = "cached_disk"
	// OutcomeExecutedRemote: at least one attempt executed on the remote
	// executor (strategy "remote").
	OutcomeExecutedRemote Outcome = "executed_remote"
	// OutcomeExecutedLocal: executed only by local strategies
	// (linux-sandbox, processwrapper-sandbox, local, worker, ...).
	OutcomeExecutedLocal Outcome = "executed_local"
	// OutcomeNotRun: the target reported no test result (failed to build,
	// build interrupted, ...).
	OutcomeNotRun Outcome = "not_run"
)

// Bazel strategy names reported in executionInfo.strategy and runnerCount.
const (
	strategyRemote         = "remote"
	strategyRemoteCacheHit = "remote cache hit"
	strategyDiskCacheHit   = "disk cache hit"
	strategyLocalCache     = "local cache"
	runnerTotal            = "total"
	runnerInternal         = "internal"
	execKindLocal          = "Local"
)

// Bazel overall test statuses that count as success.
const (
	statusPassed   = "PASSED"
	statusFlaky    = "FLAKY"
	statusNoStatus = "NO_STATUS"
)

// validationAspect is the aspect Bazel reports validation actions (nogo)
// under when --experimental_use_validation_aspect is set (.bazelrc test:ci).
// Tests then run beside validation, so a test target whose validation
// failed still reports testSummary PASSED while Bazel exits BUILD_FAILURE.
const validationAspect = "ValidateTarget"

// StatusFailedValidation replaces PASSED or FLAKY for a test target whose
// validation (nogo) failed: Bazel counts the target as failed to build.
const StatusFailedValidation = "FAILED_VALIDATION"

// Report is the JSON artifact: one entry per Bazel invocation (phase) plus
// cross-phase totals.
type Report struct {
	Schema  int          `json:"schema"`
	Context string       `json:"context"`
	Phases  []Phase      `json:"phases"`
	Totals  TargetCounts `json:"totals"`
	Slowest []SlowTest   `json:"slowest_executed"`
}

// Phase summarizes one BEP file (one `bazel test` invocation).
type Phase struct {
	Label        string         `json:"label"`
	Source       string         `json:"source"`
	Missing      bool           `json:"missing"`
	Truncated    bool           `json:"truncated"`
	InvocationID string         `json:"invocation_id"`
	Command      string         `json:"command"`
	BazelVersion string         `json:"bazel_version"`
	ExitCode     string         `json:"exit_code"`
	Targets      TargetCounts   `json:"targets"`
	Attempts     map[string]int `json:"attempts_by_strategy"`
	Actions      *ActionSummary `json:"actions"`
	Tests        []TestTarget   `json:"tests"`
	// ValidationFailed lists the targets, test or not, whose validation
	// actions (nogo) failed under the validation aspect.
	ValidationFailed []string `json:"validation_failed,omitempty"`
}

// TargetCounts are test-target tallies. Cached + Executed + NotRun == Total.
type TargetCounts struct {
	Total          int            `json:"total"`
	Cached         int            `json:"cached"`
	CachedLocal    int            `json:"cached_local"`
	CachedRemote   int            `json:"cached_remote"`
	CachedDisk     int            `json:"cached_disk"`
	Executed       int            `json:"executed"`
	ExecutedRemote int            `json:"executed_remote"`
	ExecutedLocal  int            `json:"executed_local"`
	NotRun         int            `json:"not_run"`
	Passed         int            `json:"passed"`
	Flaky          int            `json:"flaky"`
	Failed         int            `json:"failed"`
	ByStatus       map[string]int `json:"by_status"`
	ExecutedMillis int64          `json:"executed_millis"`
	CachedMillis   int64          `json:"cached_millis"`
}

// CacheHitRate is Cached / (Cached + Executed); 0 when nothing ran.
func (c TargetCounts) CacheHitRate() float64 {
	ran := c.Cached + c.Executed
	if ran == 0 {
		return 0
	}
	return float64(c.Cached) / float64(ran)
}

func (c *TargetCounts) add(o TargetCounts) {
	c.Total += o.Total
	c.Cached += o.Cached
	c.CachedLocal += o.CachedLocal
	c.CachedRemote += o.CachedRemote
	c.CachedDisk += o.CachedDisk
	c.Executed += o.Executed
	c.ExecutedRemote += o.ExecutedRemote
	c.ExecutedLocal += o.ExecutedLocal
	c.NotRun += o.NotRun
	c.Passed += o.Passed
	c.Flaky += o.Flaky
	c.Failed += o.Failed
	c.ExecutedMillis += o.ExecutedMillis
	c.CachedMillis += o.CachedMillis
	for k, v := range o.ByStatus {
		if c.ByStatus == nil {
			c.ByStatus = map[string]int{}
		}
		c.ByStatus[k] += v
	}
}

func (c *TargetCounts) count(t TestTarget) {
	c.Total++
	switch t.Outcome {
	case OutcomeCachedLocal:
		c.Cached++
		c.CachedLocal++
	case OutcomeCachedRemote:
		c.Cached++
		c.CachedRemote++
	case OutcomeCachedDisk:
		c.Cached++
		c.CachedDisk++
	case OutcomeExecutedRemote:
		c.Executed++
		c.ExecutedRemote++
	case OutcomeExecutedLocal:
		c.Executed++
		c.ExecutedLocal++
	case OutcomeNotRun:
		c.NotRun++
	}
	switch t.Status {
	case statusPassed:
		c.Passed++
	case statusFlaky:
		c.Flaky++
	default:
		c.Failed++
	}
	if c.ByStatus == nil {
		c.ByStatus = map[string]int{}
	}
	c.ByStatus[t.Status]++
	c.ExecutedMillis += t.ExecutedMillis
	c.CachedMillis += t.CachedMillis
}

// TestTarget is one test target (label + configuration) in one phase.
type TestTarget struct {
	Label      string   `json:"label"`
	Status     string   `json:"status"`
	Outcome    Outcome  `json:"outcome"`
	Strategies []string `json:"strategies"`
	// Results is the number of testResult events (runs x shards x attempts
	// reported in this invocation).
	Results        int   `json:"results"`
	Attempts       int   `json:"attempts"`
	Shards         int   `json:"shards"`
	ExecutedMillis int64 `json:"executed_millis"`
	// CachedMillis is the recorded duration of results reused from a cache:
	// test time this invocation skipped.
	CachedMillis int64 `json:"cached_millis"`
}

// ActionSummary is the action-level view from the buildMetrics event.
type ActionSummary struct {
	Created             int64    `json:"created"`
	Executed            int64    `json:"executed"`
	Runners             []Runner `json:"runners"`
	RemoteCacheHits     int64    `json:"remote_cache_hits"`
	DiskCacheHits       int64    `json:"disk_cache_hits"`
	RemoteExecuted      int64    `json:"remote_executed"`
	LocalExecuted       int64    `json:"local_executed"`
	Internal            int64    `json:"internal"`
	ActionCacheHits     int64    `json:"action_cache_hits"`
	ActionCacheMisses   int64    `json:"action_cache_misses"`
	HasActionCacheStats bool     `json:"has_action_cache_statistics"`
}

// Runner is one runnerCount entry ("remote cache hit", "linux-sandbox", ...).
type Runner struct {
	Name     string `json:"name"`
	Count    int64  `json:"count"`
	ExecKind string `json:"exec_kind"`
}

// SlowTest is one executed test in the cross-phase slowest list.
type SlowTest struct {
	Phase          string  `json:"phase"`
	Label          string  `json:"label"`
	Status         string  `json:"status"`
	Outcome        Outcome `json:"outcome"`
	Attempts       int     `json:"attempts"`
	ExecutedMillis int64   `json:"executed_millis"`
}

type targetKey struct {
	label  string
	config string
}

type targetAccumulator struct {
	results []bepTestResult
	summary *bepTestSummary
}

// SummarizePhase reads one BEP JSON stream.
func SummarizePhase(label, source string, r io.Reader) (Phase, error) {
	s, err := readStream(r)
	if err != nil {
		return Phase{}, fmt.Errorf("phase %q (%s): %w", label, source, err)
	}
	p := Phase{Label: label, Source: source, Truncated: s.truncated, Attempts: map[string]int{}}
	targets := map[targetKey]*targetAccumulator{}
	validationFailed := map[string]bool{}
	get := func(id *bepTestID) *targetAccumulator {
		k := targetKey{id.Label, id.Configuration.ID}
		acc, ok := targets[k]
		if !ok {
			acc = &targetAccumulator{}
			targets[k] = acc
		}
		return acc
	}
	for _, ev := range s.events {
		switch {
		case ev.Started != nil:
			p.InvocationID = ev.Started.UUID
			p.Command = ev.Started.Command
			p.BazelVersion = ev.Started.BuildToolVersion
		case ev.ID.TestResult != nil && ev.TestResult != nil:
			acc := get(ev.ID.TestResult)
			acc.results = append(acc.results, *ev.TestResult)
		case ev.ID.TestSummary != nil && ev.TestSummary != nil:
			get(ev.ID.TestSummary).summary = ev.TestSummary
		case ev.BuildMetrics != nil && ev.BuildMetrics.ActionSummary != nil:
			p.Actions = actionSummary(ev.BuildMetrics)
		case ev.BuildFinished != nil && ev.BuildFinished.ExitCode != nil:
			p.ExitCode = ev.BuildFinished.ExitCode.Name
		case ev.ID.TargetCompleted != nil && ev.ID.TargetCompleted.Aspect == validationAspect &&
			(ev.Completed == nil || !ev.Completed.Success):
			// An aborted completion (no completed payload) never validated
			// either.
			validationFailed[ev.ID.TargetCompleted.Label] = true
		}
	}
	for l := range validationFailed {
		p.ValidationFailed = append(p.ValidationFailed, l)
	}
	sort.Strings(p.ValidationFailed)
	if p.ExitCode == "" && finishedWithoutExitCode(s) {
		p.ExitCode = "SUCCESS"
	}
	keys := make([]targetKey, 0, len(targets))
	for k := range targets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].label != keys[j].label {
			return keys[i].label < keys[j].label
		}
		return keys[i].config < keys[j].config
	})
	for _, k := range keys {
		t := classify(k.label, targets[k])
		if validationFailed[k.label] && (t.Status == statusPassed || t.Status == statusFlaky) {
			t.Status = StatusFailedValidation
		}
		for _, r := range targets[k].results {
			p.Attempts[resultStrategy(r)]++
		}
		p.Tests = append(p.Tests, t)
		p.Targets.count(t)
	}
	return p, nil
}

// finishedWithoutExitCode reports a buildFinished event whose exitCode is omitted,
// which proto3 JSON does for the zero value SUCCESS.
func finishedWithoutExitCode(s stream) bool {
	for _, ev := range s.events {
		if ev.BuildFinished != nil {
			return ev.BuildFinished.ExitCode == nil
		}
	}
	return false
}

func actionSummary(m *bepBuildMetrics) *ActionSummary {
	in := m.ActionSummary
	a := &ActionSummary{Created: int64(in.ActionsCreated), Executed: int64(in.ActionsExecuted)}
	for _, rc := range in.RunnerCount {
		r := Runner{Name: rc.Name, Count: int64(rc.Count), ExecKind: rc.ExecKind}
		a.Runners = append(a.Runners, r)
		switch {
		case r.Name == runnerTotal:
		case r.Name == strategyRemoteCacheHit:
			a.RemoteCacheHits += r.Count
		case r.Name == strategyDiskCacheHit:
			a.DiskCacheHits += r.Count
		case r.Name == strategyRemote:
			a.RemoteExecuted += r.Count
		case r.Name == runnerInternal:
			a.Internal += r.Count
		case r.ExecKind == execKindLocal:
			a.LocalExecuted += r.Count
		}
	}
	if st := in.ActionCacheStatistics; st != nil {
		a.HasActionCacheStats = true
		a.ActionCacheHits = int64(st.Hits)
		a.ActionCacheMisses = int64(st.Misses)
	}
	return a
}

// resultStrategy names where one testResult came from: a cache tier or the
// executing strategy.
func resultStrategy(r bepTestResult) string {
	switch {
	case r.CachedLocally:
		return strategyLocalCache
	case r.ExecutionInfo.CachedRemotely && r.ExecutionInfo.Strategy != "":
		return r.ExecutionInfo.Strategy
	case r.ExecutionInfo.CachedRemotely:
		return strategyRemoteCacheHit
	case r.ExecutionInfo.Strategy != "":
		return r.ExecutionInfo.Strategy
	default:
		return "unknown"
	}
}

func classify(label string, acc *targetAccumulator) TestTarget {
	t := TestTarget{Label: label, Results: len(acc.results), Outcome: OutcomeNotRun, Status: statusNoStatus}
	if acc.summary != nil {
		if acc.summary.OverallStatus != "" {
			t.Status = acc.summary.OverallStatus
		}
		t.Attempts = int(acc.summary.AttemptCount)
		t.Shards = int(acc.summary.ShardCount)
	}
	if t.Shards == 0 {
		t.Shards = 1
	}
	if len(acc.results) == 0 {
		return t
	}
	strategies := map[string]bool{}
	var local, remoteHit, diskHit, executedRemote, executedLocal int
	for _, r := range acc.results {
		s := resultStrategy(r)
		strategies[s] = true
		switch {
		case r.CachedLocally:
			local++
			t.CachedMillis += int64(r.TestAttemptDurationMillis)
		case r.ExecutionInfo.CachedRemotely && s == strategyDiskCacheHit:
			diskHit++
			t.CachedMillis += int64(r.TestAttemptDurationMillis)
		case r.ExecutionInfo.CachedRemotely:
			remoteHit++
			t.CachedMillis += int64(r.TestAttemptDurationMillis)
		case s == strategyRemote:
			executedRemote++
			t.ExecutedMillis += int64(r.TestAttemptDurationMillis)
		default:
			executedLocal++
			t.ExecutedMillis += int64(r.TestAttemptDurationMillis)
		}
	}
	for s := range strategies {
		t.Strategies = append(t.Strategies, s)
	}
	sort.Strings(t.Strategies)
	switch {
	case executedRemote > 0:
		t.Outcome = OutcomeExecutedRemote
	case executedLocal > 0:
		t.Outcome = OutcomeExecutedLocal
	case remoteHit > 0:
		t.Outcome = OutcomeCachedRemote
	case diskHit > 0:
		t.Outcome = OutcomeCachedDisk
	case local > 0:
		t.Outcome = OutcomeCachedLocal
	}
	if t.Attempts == 0 {
		t.Attempts = len(acc.results)
	}
	return t
}

// IsExecuted reports whether the outcome ran the test rather than reusing a
// cached result.
func (o Outcome) IsExecuted() bool {
	return o == OutcomeExecutedRemote || o == OutcomeExecutedLocal
}

// BuildReport combines phases into the cross-phase report, keeping the top
// slowest executed tests.
func BuildReport(context string, phases []Phase, top int) Report {
	rep := Report{Schema: ReportSchema, Context: context, Phases: phases, Slowest: []SlowTest{}}
	for _, p := range phases {
		rep.Totals.add(p.Targets)
		for _, t := range p.Tests {
			if !t.Outcome.IsExecuted() {
				continue
			}
			rep.Slowest = append(rep.Slowest, SlowTest{
				Phase: p.Label, Label: t.Label, Status: t.Status, Outcome: t.Outcome,
				Attempts: t.Attempts, ExecutedMillis: t.ExecutedMillis,
			})
		}
	}
	sort.SliceStable(rep.Slowest, func(i, j int) bool {
		a, b := rep.Slowest[i], rep.Slowest[j]
		if a.ExecutedMillis != b.ExecutedMillis {
			return a.ExecutedMillis > b.ExecutedMillis
		}
		if a.Phase != b.Phase {
			return a.Phase < b.Phase
		}
		return a.Label < b.Label
	})
	if top >= 0 && len(rep.Slowest) > top {
		rep.Slowest = rep.Slowest[:top]
	}
	return rep
}
