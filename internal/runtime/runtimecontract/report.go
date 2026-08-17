package runtimecontract

import "github.com/gastownhall/gascity/internal/runtime"

// Status is the outcome of a single requirement check.
type Status string

// Check outcomes. Skip marks an optional requirement the executable does
// not implement; it never fails a run.
const (
	StatusPass Status = "PASS"
	StatusFail Status = "FAIL"
	StatusSkip Status = "SKIP"
)

// Result is one requirement's outcome.
type Result struct {
	Code   Code   `json:"code"`
	Group  Group  `json:"group"`
	Title  string `json:"title"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Summary aggregates a run's outcomes.
type Summary struct {
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}

// Report is the full conformance run, marshalable for `gc runtime
// conformance --json` and CI artifacts.
type Report struct {
	// Executable is the path that was checked.
	Executable string `json:"executable"`
	// Protocol is the parsed handshake; zero value when the executable has
	// no protocol op or the handshake failed.
	Protocol runtime.ProtocolInfo `json:"protocol"`
	// Results is one entry per catalog requirement, in catalog order.
	Results []Result `json:"results"`
	// Summary counts the result statuses.
	Summary Summary `json:"summary"`
}

// Failed reports whether any required requirement failed. A run with only
// passes and skips is a conformant run.
func (r Report) Failed() bool {
	return r.Summary.Failed > 0
}

func (r *Report) record(req Requirement, status Status, detail string) {
	r.Results = append(r.Results, Result{
		Code:   req.Code,
		Group:  req.Group,
		Title:  req.Title,
		Status: status,
		Detail: detail,
	})
	switch status {
	case StatusPass:
		r.Summary.Passed++
	case StatusFail:
		r.Summary.Failed++
	case StatusSkip:
		r.Summary.Skipped++
	}
}
