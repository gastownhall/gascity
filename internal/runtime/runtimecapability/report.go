package runtimecapability

// Status is the outcome of a capability probe.
type Status string

// Probe outcomes. Skip marks an undeclared capability (not advertised in the
// handshake) — reported, never a failure.
const (
	StatusPass Status = "PASS"
	StatusFail Status = "FAIL"
	StatusSkip Status = "SKIP"
)

// Result is one capability's outcome.
type Result struct {
	Code     Code   `json:"code"`
	Title    string `json:"title"`
	Declared bool   `json:"declared"`
	Status   Status `json:"status"`
	Detail   string `json:"detail,omitempty"`
}

// Summary aggregates a run.
type Summary struct {
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}

// Report is the full capability-conformance run.
type Report struct {
	Executable   string   `json:"executable"`
	Capabilities []string `json:"declaredCapabilities"`
	Results      []Result `json:"results"`
	Summary      Summary  `json:"summary"`
}

// Failed reports whether any declared capability failed its probe.
func (r Report) Failed() bool { return r.Summary.Failed > 0 }

func (r *Report) record(cb Capability, declared bool, status Status, detail string) {
	r.Results = append(r.Results, Result{
		Code: cb.Code, Title: cb.Title, Declared: declared, Status: status, Detail: detail,
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
