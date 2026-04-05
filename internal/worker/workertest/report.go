package workertest

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

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
	SuiteFailed         bool              `json:"suite_failed,omitempty"`
	FailureDetail       string            `json:"failure_detail,omitempty"`
	Profiles            int               `json:"profiles"`
	Requirements        int               `json:"requirements"`
	FailingProfiles     []ProfileID       `json:"failing_profiles,omitempty"`
	FailingRequirements []RequirementCode `json:"failing_requirements,omitempty"`
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
		}
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].Profile != results[j].Profile {
			return results[i].Profile < results[j].Profile
		}
		return results[i].Requirement < results[j].Requirement
	})

	summary.Status = summaryStatus(summary)
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
	if summary.Passed > 0 {
		return ResultPass
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
