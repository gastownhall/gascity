package workertest

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ReportSchemaVersion identifies the machine-readable conformance report schema.
const ReportSchemaVersion = "gc.worker.conformance.v1"

// RunReport is the minimal machine-readable worker-conformance run artifact.
type RunReport struct {
	SchemaVersion string            `json:"schema_version"`
	RunID         string            `json:"run_id,omitempty"`
	Suite         string            `json:"suite"`
	StartedAt     time.Time         `json:"started_at,omitempty"`
	CompletedAt   time.Time         `json:"completed_at,omitempty"`
	Elapsed       string            `json:"elapsed,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	Summary       ReportSummary     `json:"summary"`
	Results       []ReportedResult  `json:"results"`
}

// ReportSummary carries aggregate counts and top-level status.
type ReportSummary struct {
	Status              ResultStatus      `json:"status"`
	Total               int               `json:"total"`
	Passed              int               `json:"passed"`
	Failed              int               `json:"failed"`
	Unsupported         int               `json:"unsupported"`
	EnvironmentErrors   int               `json:"environment_errors,omitempty"`
	ProviderIncidents   int               `json:"provider_incidents,omitempty"`
	FlakyLive           int               `json:"flaky_live,omitempty"`
	NotCertifiableLive  int               `json:"not_certifiable_live,omitempty"`
	TopEvidence         []EvidenceDigest  `json:"top_evidence,omitempty"`
	SuiteFailed         bool              `json:"suite_failed,omitempty"`
	FailureDetail       string            `json:"failure_detail,omitempty"`
	Profiles            int               `json:"profiles"`
	Requirements        int               `json:"requirements"`
	FailingProfiles     []ProfileID       `json:"failing_profiles,omitempty"`
	FailingRequirements []RequirementCode `json:"failing_requirements,omitempty"`
}

// EvidenceDigest summarizes the most useful evidence from a result.
type EvidenceDigest struct {
	Profile     ProfileID       `json:"profile"`
	Requirement RequirementCode `json:"requirement"`
	Status      ResultStatus    `json:"status"`
	Detail      string          `json:"detail,omitempty"`
	Keys        []string        `json:"keys,omitempty"`
	Excerpt     string          `json:"excerpt,omitempty"`
}

// ReportedResult is the JSON shape for one requirement evaluation.
type ReportedResult struct {
	Requirement RequirementCode   `json:"requirement"`
	Profile     ProfileID         `json:"profile"`
	Status      ResultStatus      `json:"status"`
	Detail      string            `json:"detail,omitempty"`
	Evidence    map[string]string `json:"evidence,omitempty"`
}

// ReportInput carries the source data for a RunReport.
type ReportInput struct {
	RunID         string
	Suite         string
	StartedAt     time.Time
	CompletedAt   time.Time
	Metadata      map[string]string
	SuiteFailed   bool
	FailureDetail string
	Results       []Result
}

// NewRunReport builds a stable machine-readable report from conformance results.
func NewRunReport(input ReportInput) RunReport {
	results := make([]ReportedResult, 0, len(input.Results))
	failingProfiles := make(map[ProfileID]struct{})
	failingRequirements := make(map[RequirementCode]struct{})
	profiles := make(map[ProfileID]struct{})
	requirements := make(map[RequirementCode]struct{})

	summary := ReportSummary{}
	for _, result := range input.Results {
		results = append(results, ReportedResult{
			Requirement: result.Requirement,
			Profile:     result.Profile,
			Status:      result.Status,
			Detail:      result.Detail,
			Evidence:    copyMetadata(result.Evidence),
		})
		summary.Total++
		profiles[result.Profile] = struct{}{}
		requirements[result.Requirement] = struct{}{}
		switch result.Status {
		case ResultPass:
			summary.Passed++
		case ResultFail:
			summary.Failed++
			failingProfiles[result.Profile] = struct{}{}
			failingRequirements[result.Requirement] = struct{}{}
		case ResultUnsupported:
			summary.Unsupported++
		case ResultEnvironmentErr:
			summary.EnvironmentErrors++
		case ResultProviderIssue:
			summary.ProviderIncidents++
		case ResultFlakyLive:
			summary.FlakyLive++
		case ResultNotCertifiable:
			summary.NotCertifiableLive++
		}
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].Profile != results[j].Profile {
			return results[i].Profile < results[j].Profile
		}
		return results[i].Requirement < results[j].Requirement
	})

	summary.Status = summaryStatus(summary)
	summary.TopEvidence = topEvidence(input.Results, 5)
	if input.SuiteFailed {
		summary.SuiteFailed = true
		summary.FailureDetail = strings.TrimSpace(input.FailureDetail)
		if summary.Status != ResultFail {
			summary.Status = ResultFail
		}
	}
	summary.Profiles = len(profiles)
	summary.Requirements = len(requirements)
	summary.FailingProfiles = sortedProfileIDs(failingProfiles)
	summary.FailingRequirements = sortedRequirementCodes(failingRequirements)

	report := RunReport{
		SchemaVersion: ReportSchemaVersion,
		RunID:         input.RunID,
		Suite:         input.Suite,
		StartedAt:     input.StartedAt.UTC(),
		CompletedAt:   input.CompletedAt.UTC(),
		Metadata:      copyMetadata(input.Metadata),
		Summary:       summary,
		Results:       results,
	}
	if !input.StartedAt.IsZero() && !input.CompletedAt.IsZero() && input.CompletedAt.After(input.StartedAt) {
		report.Elapsed = input.CompletedAt.Sub(input.StartedAt).String()
	}
	return report
}

// MarshalJSON returns a stable indented JSON encoding for artifact writing.
func (r RunReport) MarshalJSON() ([]byte, error) {
	type reportAlias RunReport
	return json.Marshal(reportAlias(r))
}

// MarshalReport returns an indented JSON artifact payload.
func MarshalReport(report RunReport) ([]byte, error) {
	return json.MarshalIndent(report, "", "  ")
}

func summaryStatus(summary ReportSummary) ResultStatus {
	if summary.Failed > 0 {
		return ResultFail
	}
	if summary.FlakyLive > 0 {
		return ResultFlakyLive
	}
	if summary.ProviderIncidents > 0 {
		return ResultProviderIssue
	}
	if summary.EnvironmentErrors > 0 {
		return ResultEnvironmentErr
	}
	if summary.Passed > 0 {
		return ResultPass
	}
	if summary.NotCertifiableLive > 0 {
		return ResultNotCertifiable
	}
	if summary.Unsupported > 0 {
		return ResultUnsupported
	}
	return ResultUnsupported
}

func sortedProfileIDs(values map[ProfileID]struct{}) []ProfileID {
	out := make([]ProfileID, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortedRequirementCodes(values map[RequirementCode]struct{}) []RequirementCode {
	out := make([]RequirementCode, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func copyMetadata(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	out := make(map[string]string, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func topEvidence(results []Result, limit int) []EvidenceDigest {
	if limit <= 0 {
		return nil
	}
	digests := make([]EvidenceDigest, 0)
	for _, result := range results {
		if result.Status == ResultPass {
			continue
		}
		if len(result.Evidence) == 0 {
			continue
		}
		keys := sortedEvidenceKeys(result.Evidence)
		digests = append(digests, EvidenceDigest{
			Profile:     result.Profile,
			Requirement: result.Requirement,
			Status:      result.Status,
			Detail:      result.Detail,
			Keys:        keys,
			Excerpt:     evidenceExcerpt(result.Evidence, keys, 3),
		})
	}
	sort.Slice(digests, func(i, j int) bool {
		si := evidenceSeverity(digests[i].Status)
		sj := evidenceSeverity(digests[j].Status)
		if si != sj {
			return si < sj
		}
		if digests[i].Profile != digests[j].Profile {
			return digests[i].Profile < digests[j].Profile
		}
		return digests[i].Requirement < digests[j].Requirement
	})
	if len(digests) > limit {
		digests = digests[:limit]
	}
	if len(digests) == 0 {
		return nil
	}
	return digests
}

func sortedEvidenceKeys(evidence map[string]string) []string {
	keys := make([]string, 0, len(evidence))
	for key := range evidence {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func evidenceExcerpt(evidence map[string]string, keys []string, limit int) string {
	if len(keys) == 0 || limit <= 0 {
		return ""
	}
	if len(keys) > limit {
		keys = keys[:limit]
	}
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value := truncateEvidenceValue(evidence[key], 96)
		parts = append(parts, fmt.Sprintf("%s=%q", key, value))
	}
	return strings.Join(parts, "; ")
}

func truncateEvidenceValue(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit <= 3 {
		return string(runes[:limit])
	}
	return string(runes[:limit-3]) + "..."
}

func evidenceSeverity(status ResultStatus) int {
	switch status {
	case ResultFail:
		return 0
	case ResultEnvironmentErr:
		return 1
	case ResultProviderIssue:
		return 2
	case ResultFlakyLive:
		return 3
	case ResultNotCertifiable:
		return 4
	case ResultUnsupported:
		return 5
	default:
		return 99
	}
}
