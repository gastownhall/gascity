package packman

import (
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/gitcred"
)

// UpstreamVerdict is the closed freshness vocabulary a declared import can be
// reported under. It is deliberately small: consumers switch on it, and the
// JSON schema pins it as an enum, so a new value is a contract change.
type UpstreamVerdict string

const (
	// UpstreamCurrent means the pin already names the resolved upstream commit.
	UpstreamCurrent UpstreamVerdict = "current"
	// UpstreamBehind means upstream resolved to a different commit than the pin.
	UpstreamBehind UpstreamVerdict = "behind"
	// UpstreamUnreachable means resolution failed; the pin's freshness is
	// unknown, which is never the same answer as "current".
	UpstreamUnreachable UpstreamVerdict = "unreachable"
	// UpstreamNotApplicable means the import has no upstream to compare against
	// (a local path source, or a remote source with no packs.lock entry yet).
	UpstreamNotApplicable UpstreamVerdict = "not_applicable"
)

// UpstreamStatus is one import's freshness answer.
type UpstreamStatus struct {
	Name          string
	Source        string
	CloneURL      string
	Subpath       string
	Constraint    string
	LockedVersion string
	LockedCommit  string
	LockedFetched time.Time
	// ResolvedRef names what upstream was resolved through: a symbolic ref
	// such as "refs/heads/main" for a sha: pin, or the selected tag for a
	// semver constraint.
	ResolvedRef    string
	ResolvedCommit string
	Verdict        UpstreamVerdict
	// Err carries the resolution failure for an unreachable verdict, and the
	// explanation for a not-applicable one. A *gitcred.AuthError survives
	// wrapped so the CLI's credential hint still fires.
	Err error
}

// UpstreamReport is the freshness walk over a city's declared imports.
type UpstreamReport struct {
	// Checked is the number of declared imports examined, which is also
	// len(Statuses): every verdict, not just the ones that hit the network.
	Checked  int
	Statuses []UpstreamStatus // sorted by Name
}

// Count returns how many statuses carry verdict v.
func (r *UpstreamReport) Count(v UpstreamVerdict) int {
	if r == nil {
		return 0
	}
	count := 0
	for _, status := range r.Statuses {
		if status.Verdict == v {
			count++
		}
	}
	return count
}

// Behind returns the statuses whose pin is behind upstream.
func (r *UpstreamReport) Behind() []UpstreamStatus {
	return r.withVerdict(UpstreamBehind)
}

// Unreachable returns the statuses whose upstream could not be resolved.
func (r *UpstreamReport) Unreachable() []UpstreamStatus {
	return r.withVerdict(UpstreamUnreachable)
}

func (r *UpstreamReport) withVerdict(v UpstreamVerdict) []UpstreamStatus {
	if r == nil {
		return nil
	}
	var out []UpstreamStatus
	for _, status := range r.Statuses {
		if status.Verdict == v {
			out = append(out, status)
		}
	}
	return out
}

// CheckUpstream resolves each declared import's source and compares it against
// the packs.lock pin. Unlike CheckInstalled it performs network operations, so
// it is a sibling entry point rather than an extension of the offline walk:
// nothing that calls CheckInstalled gains a network dependency by its
// existence. A nil lock is read from cityRoot.
//
// It returns an error only when it cannot read its own inputs. A per-import
// resolution failure is data (UpstreamUnreachable), never a returned error --
// otherwise one dead remote hides the verdict for every other import.
func CheckUpstream(cityRoot string, imports map[string]config.Import, lock *Lockfile) (*UpstreamReport, error) {
	if lock == nil {
		var err error
		lock, err = ReadLockfile(fsys.OSFS{}, cityRoot)
		if err != nil {
			return nil, err
		}
	}

	resolver := &upstreamResolver{
		cityRoot: cityRoot,
		heads:    make(map[string]upstreamResolution),
		versions: make(map[string]upstreamResolution),
	}
	report := &UpstreamReport{}
	for _, name := range sortedImportNames(imports) {
		report.Statuses = append(report.Statuses, resolver.status(name, imports[name], lock))
	}
	report.Checked = len(report.Statuses)
	return report, nil
}

func (r *upstreamResolver) status(name string, imp config.Import, lock *Lockfile) UpstreamStatus {
	status := UpstreamStatus{
		Name:       name,
		Source:     imp.Source,
		Constraint: imp.Version,
	}

	// A scheme-less path source has no upstream to resolve. Note that a
	// file:// source is *not* one of these: remotesource.IsRemote treats it as
	// remote and ls-remote works against it, so it takes the network path and
	// reaches a real verdict like any other remote.
	if !isRemoteSource(imp.Source) {
		status.Verdict = UpstreamNotApplicable
		return status
	}

	parsed := normalizeRemoteSource(imp.Source)
	status.CloneURL = parsed.CloneURL
	status.Subpath = parsed.Subpath

	locked, ok := lock.Packs[imp.Source]
	if !ok {
		// A missing pin is CheckInstalled's missing-lock-entry to report. The
		// two walks must not double-blame the same defect, so this one stops
		// at not-applicable with an explanation rather than a verdict.
		status.Verdict = UpstreamNotApplicable
		status.Err = fmt.Errorf("no %s entry for %q; run \"gc import install\"",
			LockfileName, gitcred.RedactUserinfo(imp.Source))
		return status
	}
	status.LockedVersion = locked.Version
	status.LockedCommit = locked.Commit
	status.LockedFetched = locked.Fetched

	ref, commit, err := r.resolve(imp)
	if err != nil {
		status.Verdict = UpstreamUnreachable
		status.Err = err
		return status
	}
	status.ResolvedRef = ref
	status.ResolvedCommit = commit
	if commit == status.LockedCommit {
		status.Verdict = UpstreamCurrent
	} else {
		status.Verdict = UpstreamBehind
	}
	return status
}

// upstreamResolver memoizes resolution within one CheckUpstream call. The key
// is the clone URL, not the import: two imports of different subpaths of the
// same repository share one round trip, because freshness is a property of the
// repository.
type upstreamResolver struct {
	cityRoot string
	heads    map[string]upstreamResolution
	versions map[string]upstreamResolution
}

type upstreamResolution struct {
	ref    string
	commit string
	err    error
}

func (r *upstreamResolver) resolve(imp config.Import) (string, string, error) {
	cloneURL := normalizeRemoteSource(imp.Source).CloneURL

	// A sha: constraint pins a fixed commit, so ResolveVersion short-circuits
	// and echoes it back -- comparing that against the pin would report every
	// sha: import current no matter how far upstream had moved. The question
	// worth asking for a sha: pin is what the source's default branch points at
	// now, which is the one genuinely new network call in this package.
	if strings.HasPrefix(imp.Version, "sha:") {
		res, ok := r.heads[cloneURL]
		if !ok {
			res.ref, res.commit, res.err = resolveSourceHead(r.cityRoot, imp.Source)
			r.heads[cloneURL] = res
		}
		return res.ref, res.commit, res.err
	}

	key := cloneURL + "\x00" + imp.Version
	res, ok := r.versions[key]
	if !ok {
		resolved, err := ResolveVersion(r.cityRoot, imp.Source, imp.Version)
		res = upstreamResolution{ref: resolved.Version, commit: resolved.Commit, err: err}
		r.versions[key] = res
	}
	return res.ref, res.commit, res.err
}

// resolveSourceHead reports the ref and commit the source's HEAD points at.
//
// It goes through runNetworkGit rather than exec'ing git directly, which is
// what makes it inherit credential injection, the SSRF/transport hardening,
// the packman-local network timeout, and the existing test seam.
func resolveSourceHead(cityRoot, source string) (string, string, error) {
	cloneURL := normalizeRemoteSource(source).CloneURL
	out, err := runNetworkGit(cityRoot, cloneURL, "", "ls-remote", "--symref", cloneURL, "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("resolving head for %q: %w", gitcred.RedactUserinfo(source), err)
	}
	ref, commit := parseSymrefHead(out)
	if commit == "" {
		return "", "", fmt.Errorf("resolving head for %q: no HEAD in ls-remote output",
			gitcred.RedactUserinfo(source))
	}
	return ref, commit, nil
}

// parseSymrefHead extracts the default-branch ref and head commit from an
// `ls-remote --symref <url> HEAD` response.
//
// The response is two lines against a normal https remote, but a file:// clone
// of a *non-bare* repository also advertises its remote-tracking refs, and the
// HEAD refspec glob-matches refs/remotes/origin/HEAD -- so the same request can
// come back with four lines and a second, different sha:
//
//	ref: refs/heads/main\tHEAD
//	<head-sha>\tHEAD
//	ref: refs/remotes/origin/main\trefs/remotes/origin/HEAD
//	<tracking-sha>\trefs/remotes/origin/HEAD
//
// Only a line whose second tab-separated field is exactly "HEAD" describes the
// repository's own head. Matching on a "HEAD" suffix, or taking the last
// matching line, silently reports the remote-tracking sha instead and calls an
// up-to-date import behind.
//
// A source that advertises no symref line yields an empty ref and the commit
// alone, which is enough to reach a verdict.
func parseSymrefHead(out string) (string, string) {
	ref, commit := "", ""
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) != 2 || fields[1] != "HEAD" {
			continue
		}
		value := strings.TrimSpace(fields[0])
		if rest, found := strings.CutPrefix(value, "ref:"); found {
			if ref == "" {
				ref = strings.TrimSpace(rest)
			}
			continue
		}
		if commit == "" {
			commit = value
		}
	}
	return ref, commit
}
