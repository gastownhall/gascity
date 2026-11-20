package main

import (
	"fmt"
	goruntime "runtime"

	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/execenv"
)

// bdBatchFileFixHint is the remedy for a bd that resolves to a batch file.
const bdBatchFileFixHint = "install bd's native executable (bd.exe) and put it first on PATH, or pin it with [workspace.env] BD_BIN"

// bdBatchFileProblem describes what a bd that resolves to a batch file costs,
// or returns "" when the bd at bdPath is not a batch file. Windows runs a batch
// file through cmd.exe, so gc refuses to pass such a bd any text cmd.exe acts
// on (execenv.CheckBatchFileArgs); without this up-front note, the first sign
// is a failed write.
func bdBatchFileProblem(bdPath string) string {
	if bdPath == "" || !execenv.IsBatchFile(bdPath) {
		return ""
	}
	return fmt.Sprintf("bd resolves to the batch file %s, which Windows runs through cmd.exe, so gc refuses to pass it text holding \" %% ! ^ & | < > ( ) or a line break: a bead or mail write whose title, description, notes or body holds one fails, including any multi-line text", bdPath)
}

// bdBatchFileWarning is bdBatchFileProblem with its remedy, for one-line
// warnings.
func bdBatchFileWarning(bdPath string) string {
	problem := bdBatchFileProblem(bdPath)
	if problem == "" {
		return ""
	}
	return problem + "; " + bdBatchFileFixHint
}

// bdBatchFileCheckApplies reports whether the city at cityPath runs bd on a
// platform where bd can be a batch file.
func bdBatchFileCheckApplies(cityPath string) bool {
	return goruntime.GOOS == "windows" && initNeedsBdTooling(cityPath)
}

// startBdBatchFileWarning is the gc start warning for the city's bd, or "".
func startBdBatchFileWarning(cityPath string) string {
	if !bdBatchFileCheckApplies(cityPath) {
		return ""
	}
	bdPath, err := resolveBdBinaryForScope(cityPath, cityPath)
	if err != nil {
		return ""
	}
	return bdBatchFileWarning(bdPath)
}

// bdBatchFileCheck warns when the city's bd resolves to a batch file.
type bdBatchFileCheck struct {
	resolve func() (string, error)
}

func newBdBatchFileCheck(cityPath string) *bdBatchFileCheck {
	return &bdBatchFileCheck{resolve: func() (string, error) { return resolveBdBinaryForScope(cityPath, cityPath) }}
}

// Name returns the check identifier.
func (c *bdBatchFileCheck) Name() string { return "bd-batch-file" }

// Run resolves the city's bd and warns when it is a batch file. A bd that
// does not resolve is another check's finding.
func (c *bdBatchFileCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK}
	bdPath, err := c.resolve()
	if err != nil {
		r.Message = fmt.Sprintf("skipped: %v", err)
		return r
	}
	if problem := bdBatchFileProblem(bdPath); problem != "" {
		r.Status = doctor.StatusWarning
		r.Message = problem
		r.FixHint = bdBatchFileFixHint
		return r
	}
	r.Message = fmt.Sprintf("%s is not a batch file", bdPath)
	return r
}

// CanFix returns false.
func (c *bdBatchFileCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *bdBatchFileCheck) Fix(_ *doctor.CheckContext) error { return nil }

// WarmupEligible returns false: gc start prints its own warning.
func (c *bdBatchFileCheck) WarmupEligible() bool { return false }
