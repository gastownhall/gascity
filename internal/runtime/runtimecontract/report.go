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
	// V2Ready says the executable is fit for reconciler v2: its handshake
	// declares what v2 needs from an exec runtime (V2Missing names what it
	// lacks, exec.V2ProfileGaps) and it failed no requirement (V2Failed)
	// but an advisory one, whose FAIL is in Results alone.
	V2Ready   bool     `json:"v2_ready"`
	V2Missing []string `json:"v2_missing,omitempty"`
	V2Failed  []Code   `json:"v2_failed,omitempty"`
	// Results is one entry per catalog requirement, in catalog order.
	Results []Result `json:"results"`
	// Summary counts the result statuses.
	Summary Summary `json:"summary"`
	// Warnings name what the run could not clean up and did not gate: a
	// final server-stop in a private city that did not exit 0 before the
	// city was removed.
	Warnings []string `json:"warnings,omitempty"`
}

// Failed reports whether any required requirement failed. A run with only
// passes, skips and advisory FAILs is a conformant run.
func (r Report) Failed() bool {
	return r.RequiredFailed() > 0
}

// RequiredFailed counts the FAILs that fail the run: every FAIL but an
// advisory group's, which Summary.Failed still counts.
func (r Report) RequiredFailed() int {
	n := 0
	for _, res := range r.Results {
		if res.Status == StatusFail && !advisory(res.Group) {
			n++
		}
	}
	return n
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
