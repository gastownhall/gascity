package workertest

import "fmt"

// ResultStatus is the outcome of a conformance check.
type ResultStatus string

const (
	ResultPass        ResultStatus = "pass"
	ResultFail        ResultStatus = "fail"
	ResultUnsupported ResultStatus = "unsupported"
)

// Result captures one requirement evaluation.
type Result struct {
	Requirement RequirementCode
	Profile     ProfileID
	Status      ResultStatus
	Detail      string
	Evidence    map[string]string
}

// Passed returns whether the result is passing.
func (r Result) Passed() bool {
	return r.Status == ResultPass
}

// Err converts a failing result into an error.
func (r Result) Err() error {
	if r.Passed() {
		return nil
	}
	return fmt.Errorf("%s %s: %s", r.Profile, r.Requirement, r.Detail)
}

// Pass returns a passing result helper.
func Pass(profile ProfileID, requirement RequirementCode, detail string) Result {
	return Result{
		Requirement: requirement,
		Profile:     profile,
		Status:      ResultPass,
		Detail:      detail,
	}
}

// Fail returns a failing result helper.
func Fail(profile ProfileID, requirement RequirementCode, detail string) Result {
	return Result{
		Requirement: requirement,
		Profile:     profile,
		Status:      ResultFail,
		Detail:      detail,
	}
}

// Unsupported returns an unsupported result helper.
func Unsupported(profile ProfileID, requirement RequirementCode, detail string) Result {
	return Result{
		Requirement: requirement,
		Profile:     profile,
		Status:      ResultUnsupported,
		Detail:      detail,
	}
}

// WithEvidence returns a copy of the result with structured evidence attached.
func (r Result) WithEvidence(evidence map[string]string) Result {
	r.Evidence = copyMetadata(evidence)
	return r
}
