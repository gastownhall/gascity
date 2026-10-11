package main

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	gitutil "github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/remotesource"
)

const nestedPackCommitsCheckName = "nested-pack-commits"

// nestedPackCommitsDoctorCheck reports packs.lock entries where one locked
// pack lives inside another locked pack's directory in the same repository
// (for example "<repo>/gascity" and "<repo>/gascity/roles") and the two are
// locked at different commits. A nested pack ships in its parent's tree, so
// splitting them runs the parent's commands and the nested pack's prompts
// from two different releases. packs.lock keys entries by source, so nothing
// else ties the two commits together.
//
// Sibling packs in one repository are not compared; they are independent
// packs that happen to share a repository. The check is advisory and
// offline: it reads packs.lock only.
type nestedPackCommitsDoctorCheck struct {
	cityPath string
}

func newNestedPackCommitsDoctorCheck(cityPath string) *nestedPackCommitsDoctorCheck {
	return &nestedPackCommitsDoctorCheck{cityPath: cityPath}
}

// Name implements doctor.Check.
func (*nestedPackCommitsDoctorCheck) Name() string { return nestedPackCommitsCheckName }

// CanFix implements doctor.Check. Choosing which commit both packs move to
// is the operator's decision.
func (*nestedPackCommitsDoctorCheck) CanFix() bool { return false }

// Fix implements doctor.Check.
func (*nestedPackCommitsDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

// WarmupEligible implements doctor.Check.
func (*nestedPackCommitsDoctorCheck) WarmupEligible() bool { return false }

// nestedPackCommitsPayload is the --json payload of nested-pack-commits.
type nestedPackCommitsPayload struct {
	Splits []nestedPackSplit `json:"splits"`
}

// nestedPackSplit is one parent/nested pair locked at different commits.
type nestedPackSplit struct {
	Repository   string `json:"repository"`
	ParentSource string `json:"parent_source"`
	ParentCommit string `json:"parent_commit"`
	NestedSource string `json:"nested_source"`
	NestedCommit string `json:"nested_commit"`
}

// Run implements doctor.Check.
func (c *nestedPackCommitsDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: nestedPackCommitsCheckName, Severity: doctor.SeverityAdvisory}
	lock, err := readImportLockfile(fsys.OSFS{}, c.cityPath)
	if err != nil {
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("reading packs.lock: %v", err)
		return r
	}

	type locked struct {
		source, subpath, commit string
	}
	byRepo := make(map[string][]locked)
	for source, pack := range lock.Packs {
		if !remotesource.IsRemote(source) || strings.TrimSpace(pack.Commit) == "" {
			continue
		}
		parsed := remotesource.Parse(source)
		repo := normalizeLockRepository(parsed.CloneURL)
		byRepo[repo] = append(byRepo[repo], locked{source: source, subpath: parsed.Subpath, commit: strings.TrimSpace(pack.Commit)})
	}

	var splits []nestedPackSplit
	for repo, packs := range byRepo {
		for _, parent := range packs {
			for _, nested := range packs {
				if parent.source == nested.source || !subpathContains(parent.subpath, nested.subpath) {
					continue
				}
				if gitutil.SameCommit(parent.commit, nested.commit) || gitutil.SameCommit(nested.commit, parent.commit) {
					continue
				}
				splits = append(splits, nestedPackSplit{
					Repository:   repo,
					ParentSource: parent.source,
					ParentCommit: parent.commit,
					NestedSource: nested.source,
					NestedCommit: nested.commit,
				})
			}
		}
	}
	if len(splits) == 0 {
		r.Status = doctor.StatusOK
		r.Message = "every nested pack is locked at its parent pack's commit"
		return r
	}
	sort.Slice(splits, func(i, j int) bool {
		if splits[i].ParentSource != splits[j].ParentSource {
			return splits[i].ParentSource < splits[j].ParentSource
		}
		return splits[i].NestedSource < splits[j].NestedSource
	})
	for _, s := range splits {
		r.Details = append(r.Details, fmt.Sprintf("split | %s @ %s | %s @ %s", s.ParentSource, shortCommit(s.ParentCommit), s.NestedSource, shortCommit(s.NestedCommit)))
	}
	r.Payload = nestedPackCommitsPayload{Splits: splits}
	r.Status = doctor.StatusWarning
	r.Message = fmt.Sprintf("%d nested pack(s) locked at a different commit than the pack that contains them", len(splits))
	r.FixHint = `declare the same version on both imports, then run "gc import install"`
	return r
}

// subpathContains reports whether nested lies strictly inside parent. An
// empty parent is the repository root, which contains every subpath.
func subpathContains(parent, nested string) bool {
	parent = strings.Trim(path.Clean("/"+parent), "/")
	nested = strings.Trim(path.Clean("/"+nested), "/")
	if nested == parent || nested == "" {
		return false
	}
	return parent == "" || strings.HasPrefix(nested, parent+"/")
}

// normalizeLockRepository folds the trailing-slash and ".git" spellings of
// one repository's clone URL to one key.
func normalizeLockRepository(cloneURL string) string {
	return strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(cloneURL), "/"), ".git")
}
