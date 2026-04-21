package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
)

type importStateDoctorCheck struct {
	cityPath string
}

func newImportStateDoctorCheck(cityPath string) *importStateDoctorCheck {
	return &importStateDoctorCheck{cityPath: cityPath}
}

func (c *importStateDoctorCheck) Name() string { return "packv2-import-state" }

func (c *importStateDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}

	imports, err := collectAllImportsFS(fsys.OSFS{}, c.cityPath)
	if err != nil {
		r.Status = doctor.StatusError
		r.Message = fmt.Sprintf("reading declared imports: %v", err)
		return r
	}
	report, err := checkInstalledImports(c.cityPath, imports)
	if err != nil {
		r.Status = doctor.StatusError
		r.Message = fmt.Sprintf("checking import state: %v", err)
		r.FixHint = `run "gc import install"`
		return r
	}
	if !report.HasIssues() {
		r.Status = doctor.StatusOK
		r.Message = fmt.Sprintf("%d remote import(s) installed", report.CheckedSources)
		return r
	}

	r.Status = doctor.StatusError
	r.Message = fmt.Sprintf("%d import state issue(s)", len(report.Issues))
	r.FixHint = `run "gc import install"`
	for _, issue := range report.Issues {
		r.Details = append(r.Details, formatImportStateDoctorDetail(issue))
	}
	return r
}

func (c *importStateDoctorCheck) CanFix() bool { return false }

func (c *importStateDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

func formatImportStateDoctorDetail(issue packman.CheckIssue) string {
	parts := []string{issue.Code}
	if issue.ImportName != "" {
		parts = append(parts, issue.ImportName)
	}
	if issue.Source != "" {
		parts = append(parts, issue.Source)
	}
	if issue.Commit != "" {
		parts = append(parts, "commit="+issue.Commit)
	}
	if issue.Path != "" {
		parts = append(parts, "path="+issue.Path)
	}
	if issue.Message != "" {
		parts = append(parts, issue.Message)
	}
	return strings.Join(parts, " | ")
}
