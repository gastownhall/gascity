package packman

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/gitcred"
)

const (
	upstreamToolsSource = "https://example.com/tools.git"
	upstreamLockedHead  = "1111111111111111111111111111111111111111"
	upstreamMovedHead   = "2222222222222222222222222222222222222222"
)

// TestUpstreamFreshnessSeesWhatCheckInstalledCannot is the non-vacuity proof
// for the whole freshness feature. On one fixture city whose on-disk state
// never changes between the two halves, the offline walk reports a perfectly
// healthy import state and the upstream walk reports that same import behind.
//
// Both assertions stay in one function on purpose: the contrast is the test.
// Split across two, a later refactor can delete the half that hurts and leave
// the other one green, and the check goes back to being vacuous without
// anything turning red.
func TestUpstreamFreshnessSeesWhatCheckInstalledCannot(t *testing.T) {
	city := newUpstreamFixtureCity(t)
	imports := map[string]config.Import{
		"pack:tools": {Source: upstreamToolsSource, Version: "sha:" + upstreamLockedHead},
	}
	writeTestLockfile(t, city, map[string]LockedPack{
		upstreamToolsSource: {Version: "sha:" + upstreamLockedHead, Commit: upstreamLockedHead},
	})
	stageCachedPack(t, upstreamToolsSource, upstreamLockedHead, "\n[pack]\nname = \"tools\"\nschema = 1\n")

	installed, err := CheckInstalled(city, imports)
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	if installed.HasIssues() {
		t.Fatalf("CheckInstalled issues = %#v, want none: declared, locked, and materialized all agree", installed.Issues)
	}

	// Same unchanged on-disk state. Only upstream has moved.
	stubUpstreamNetworkGit(t, func([]string) (string, error) {
		return symrefHeadResponse("refs/heads/main", upstreamMovedHead), nil
	})

	report, err := CheckUpstream(city, imports, nil)
	if err != nil {
		t.Fatalf("CheckUpstream: %v", err)
	}
	status := findUpstreamStatus(t, report, "pack:tools")
	if status.Verdict != UpstreamBehind {
		t.Fatalf("verdict = %q, want %q; err=%v", status.Verdict, UpstreamBehind, status.Err)
	}
	if status.LockedCommit != upstreamLockedHead {
		t.Fatalf("LockedCommit = %q, want %q", status.LockedCommit, upstreamLockedHead)
	}
	if status.ResolvedCommit != upstreamMovedHead {
		t.Fatalf("ResolvedCommit = %q, want %q", status.ResolvedCommit, upstreamMovedHead)
	}
	if status.ResolvedRef != "refs/heads/main" {
		t.Fatalf("ResolvedRef = %q, want refs/heads/main", status.ResolvedRef)
	}
}

// TestCheckUpstreamResolvesEachConstraintKind covers both constraint kinds in
// both directions. The current cases are the load-bearing ones: a walk that
// answered "behind" unconditionally would satisfy the behind rows alone.
func TestCheckUpstreamResolvesEachConstraintKind(t *testing.T) {
	const (
		tagCommit147 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		tagCommit200 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	tags := fmt.Sprintf("%s\trefs/tags/v1.4.7\n%s\trefs/tags/v2.0.0\n", tagCommit147, tagCommit200)

	tests := []struct {
		name        string
		constraint  string
		locked      string
		wantVerdict UpstreamVerdict
		wantRef     string
		wantCommit  string
	}{
		{
			name:        "sha pin equal to the source head is current",
			constraint:  "sha:" + upstreamLockedHead,
			locked:      upstreamLockedHead,
			wantVerdict: UpstreamCurrent,
			wantRef:     "refs/heads/main",
			wantCommit:  upstreamLockedHead,
		},
		{
			name:        "sha pin behind the source head is behind",
			constraint:  "sha:" + upstreamMovedHead,
			locked:      upstreamMovedHead,
			wantVerdict: UpstreamBehind,
			wantRef:     "refs/heads/main",
			wantCommit:  upstreamLockedHead,
		},
		{
			name:        "semver pin at the highest matching tag is current",
			constraint:  "^1.4",
			locked:      tagCommit147,
			wantVerdict: UpstreamCurrent,
			wantRef:     "1.4.7",
			wantCommit:  tagCommit147,
		},
		{
			name:        "semver pin below the highest matching tag is behind",
			constraint:  "^1.4",
			locked:      "cccccccccccccccccccccccccccccccccccccccc",
			wantVerdict: UpstreamBehind,
			wantRef:     "1.4.7",
			wantCommit:  tagCommit147,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			city := newUpstreamFixtureCity(t)
			stubUpstreamNetworkGit(t, func(args []string) (string, error) {
				return upstreamFixtureResponse(args, symrefHeadResponse("refs/heads/main", upstreamLockedHead), tags)
			})
			writeTestLockfile(t, city, map[string]LockedPack{
				upstreamToolsSource: {Version: tt.constraint, Commit: tt.locked},
			})

			report, err := CheckUpstream(city, map[string]config.Import{
				"pack:tools": {Source: upstreamToolsSource, Version: tt.constraint},
			}, nil)
			if err != nil {
				t.Fatalf("CheckUpstream: %v", err)
			}
			status := findUpstreamStatus(t, report, "pack:tools")
			if status.Verdict != tt.wantVerdict {
				t.Fatalf("verdict = %q, want %q; err=%v", status.Verdict, tt.wantVerdict, status.Err)
			}
			if status.ResolvedRef != tt.wantRef {
				t.Fatalf("ResolvedRef = %q, want %q", status.ResolvedRef, tt.wantRef)
			}
			if status.ResolvedCommit != tt.wantCommit {
				t.Fatalf("ResolvedCommit = %q, want %q", status.ResolvedCommit, tt.wantCommit)
			}
		})
	}
}

// TestCheckUpstreamParsesFileSourceSymrefWithRemoteTrackingRefs pins T-1.
//
// `ls-remote --symref <file:// url> HEAD` against a clone of a *non-bare*
// repository returns four lines, not two: the clone advertises its
// remote-tracking refs and the HEAD refspec glob-matches
// refs/remotes/origin/HEAD, whose sha is a different commit. A parser that
// suffix-matches "HEAD", or takes the last matching line, reads that second
// sha and calls an up-to-date import behind.
func TestCheckUpstreamParsesFileSourceSymrefWithRemoteTrackingRefs(t *testing.T) {
	const (
		source       = "file:///gc/apicity"
		realHead     = "bd29eb3830f4da727f5d1184092192d5dec29142"
		trackingHead = "4999445bdd5f5695f67ea182eee69f60e0187598"
	)
	city := newUpstreamFixtureCity(t)
	stubUpstreamNetworkGit(t, func([]string) (string, error) {
		return strings.Join([]string{
			"ref: refs/heads/main\tHEAD",
			realHead + "\tHEAD",
			"ref: refs/remotes/origin/main\trefs/remotes/origin/HEAD",
			trackingHead + "\trefs/remotes/origin/HEAD",
		}, "\n") + "\n", nil
	})
	writeTestLockfile(t, city, map[string]LockedPack{
		source: {Version: "sha:" + realHead, Commit: realHead},
	})

	report, err := CheckUpstream(city, map[string]config.Import{
		"pack:apicity-release": {Source: source, Version: "sha:" + realHead},
	}, nil)
	if err != nil {
		t.Fatalf("CheckUpstream: %v", err)
	}
	status := findUpstreamStatus(t, report, "pack:apicity-release")
	if status.ResolvedCommit == trackingHead {
		t.Fatalf("resolved the refs/remotes/origin/HEAD sha %q instead of the repository head", trackingHead)
	}
	if status.ResolvedCommit != realHead {
		t.Fatalf("ResolvedCommit = %q, want %q", status.ResolvedCommit, realHead)
	}
	if status.ResolvedRef != "refs/heads/main" {
		t.Fatalf("ResolvedRef = %q, want refs/heads/main", status.ResolvedRef)
	}
	if status.Verdict != UpstreamCurrent {
		t.Fatalf("verdict = %q, want %q", status.Verdict, UpstreamCurrent)
	}
}

// TestCheckUpstreamTreatsFileSourceAsRemote pins T-2. A file:// source is a
// remote source -- remotesource.IsRemote says so and ls-remote works against
// it -- so it must reach a real verdict. The not-applicable branch is for
// scheme-less path sources, and routing file:// there would silently drop the
// one import most likely to be reported current.
func TestCheckUpstreamTreatsFileSourceAsRemote(t *testing.T) {
	city := newUpstreamFixtureCity(t)
	pathSource := writeLocalPack(t, "[pack]\nname = \"local\"\nschema = 1\n")
	calls := stubUpstreamNetworkGit(t, func([]string) (string, error) {
		return symrefHeadResponse("refs/heads/main", upstreamMovedHead), nil
	})
	writeTestLockfile(t, city, map[string]LockedPack{
		"file:///srv/pack": {Version: "sha:" + upstreamLockedHead, Commit: upstreamLockedHead},
	})

	report, err := CheckUpstream(city, map[string]config.Import{
		"pack:file":  {Source: "file:///srv/pack", Version: "sha:" + upstreamLockedHead},
		"pack:local": {Source: pathSource},
	}, nil)
	if err != nil {
		t.Fatalf("CheckUpstream: %v", err)
	}
	if got := findUpstreamStatus(t, report, "pack:file").Verdict; got != UpstreamBehind {
		t.Fatalf("file:// verdict = %q, want %q: file:// is a remote source", got, UpstreamBehind)
	}
	if got := findUpstreamStatus(t, report, "pack:local").Verdict; got != UpstreamNotApplicable {
		t.Fatalf("path verdict = %q, want %q", got, UpstreamNotApplicable)
	}
	if len(*calls) != 1 {
		t.Fatalf("network calls = %v, want exactly the file:// resolution", *calls)
	}
}

// TestCheckUpstreamFallsBackWhenSymrefAbsent covers OQ-3: a source that
// advertises no symref line still yields a usable verdict from the commit line
// alone, with an empty ref rather than a failure.
func TestCheckUpstreamFallsBackWhenSymrefAbsent(t *testing.T) {
	city := newUpstreamFixtureCity(t)
	stubUpstreamNetworkGit(t, func([]string) (string, error) {
		return upstreamMovedHead + "\tHEAD\n", nil
	})
	writeTestLockfile(t, city, map[string]LockedPack{
		upstreamToolsSource: {Version: "sha:" + upstreamLockedHead, Commit: upstreamLockedHead},
	})

	report, err := CheckUpstream(city, map[string]config.Import{
		"pack:tools": {Source: upstreamToolsSource, Version: "sha:" + upstreamLockedHead},
	}, nil)
	if err != nil {
		t.Fatalf("CheckUpstream: %v", err)
	}
	status := findUpstreamStatus(t, report, "pack:tools")
	if status.Verdict != UpstreamBehind {
		t.Fatalf("verdict = %q, want %q; err=%v", status.Verdict, UpstreamBehind, status.Err)
	}
	if status.ResolvedRef != "" {
		t.Fatalf("ResolvedRef = %q, want empty", status.ResolvedRef)
	}
	if status.ResolvedCommit != upstreamMovedHead {
		t.Fatalf("ResolvedCommit = %q, want %q", status.ResolvedCommit, upstreamMovedHead)
	}
}

// TestCheckUpstreamReportsUnlockedSourceNotApplicable keeps the two walks from
// double-blaming one defect: a declared remote with no packs.lock entry is
// CheckInstalled's missing-lock-entry to report, so this walk stops at
// not-applicable and explains itself rather than issuing a freshness verdict
// it has no pin to compare against.
func TestCheckUpstreamReportsUnlockedSourceNotApplicable(t *testing.T) {
	city := newUpstreamFixtureCity(t)
	calls := stubUpstreamNetworkGit(t, func([]string) (string, error) {
		return "", fmt.Errorf("unlocked sources must not reach the network")
	})
	writeTestLockfile(t, city, map[string]LockedPack{})

	report, err := CheckUpstream(city, map[string]config.Import{
		"pack:tools": {Source: upstreamToolsSource, Version: "^1.0"},
	}, nil)
	if err != nil {
		t.Fatalf("CheckUpstream: %v", err)
	}
	status := findUpstreamStatus(t, report, "pack:tools")
	if status.Verdict != UpstreamNotApplicable {
		t.Fatalf("verdict = %q, want %q", status.Verdict, UpstreamNotApplicable)
	}
	if status.Err == nil || !strings.Contains(status.Err.Error(), "gc import install") {
		t.Fatalf("Err = %v, want an explanation naming \"gc import install\"", status.Err)
	}
	if len(*calls) != 0 {
		t.Fatalf("network calls = %v, want none", *calls)
	}
}

// TestCheckUpstreamReportsResolutionFailureUnreachable asserts a dead remote
// is reported unreachable rather than falling through to current, that it does
// not fail the whole walk, and that a typed *gitcred.AuthError survives the
// wrap so the CLI still prints its credential hint (TS-3).
func TestCheckUpstreamReportsResolutionFailureUnreachable(t *testing.T) {
	const liveSource = "https://example.com/live.git"
	city := newUpstreamFixtureCity(t)
	authErr := &gitcred.AuthError{Host: "example.com", OrgPrefix: "example.com/dead", Repo: "https://example.com/dead.git"}
	stubUpstreamNetworkGit(t, func(args []string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "dead.git") {
			return "", authErr
		}
		return symrefHeadResponse("refs/heads/main", upstreamLockedHead), nil
	})
	writeTestLockfile(t, city, map[string]LockedPack{
		"https://example.com/dead.git": {Version: "sha:" + upstreamLockedHead, Commit: upstreamLockedHead},
		liveSource:                     {Version: "sha:" + upstreamLockedHead, Commit: upstreamLockedHead},
	})

	report, err := CheckUpstream(city, map[string]config.Import{
		"pack:dead": {Source: "https://example.com/dead.git", Version: "sha:" + upstreamLockedHead},
		"pack:live": {Source: liveSource, Version: "sha:" + upstreamLockedHead},
	}, nil)
	if err != nil {
		t.Fatalf("CheckUpstream returned an error for a per-import failure: %v", err)
	}
	dead := findUpstreamStatus(t, report, "pack:dead")
	if dead.Verdict != UpstreamUnreachable {
		t.Fatalf("verdict = %q, want %q", dead.Verdict, UpstreamUnreachable)
	}
	if dead.ResolvedCommit != "" {
		t.Fatalf("ResolvedCommit = %q, want empty for an unresolved source", dead.ResolvedCommit)
	}
	var got *gitcred.AuthError
	if !errors.As(dead.Err, &got) {
		t.Fatalf("Err = %v, want a wrapped *gitcred.AuthError", dead.Err)
	}
	// One dead remote must not hide every other verdict.
	if live := findUpstreamStatus(t, report, "pack:live").Verdict; live != UpstreamCurrent {
		t.Fatalf("live verdict = %q, want %q", live, UpstreamCurrent)
	}
	if n := report.Count(UpstreamUnreachable); n != 1 {
		t.Fatalf("Count(unreachable) = %d, want 1", n)
	}
	if n := len(report.Unreachable()); n != 1 {
		t.Fatalf("len(Unreachable()) = %d, want 1", n)
	}
}

// TestCheckUpstreamMemoizesByCloneURL covers OQ-5: freshness is a property of
// the repository, so two subpath imports of one repository cost one round trip.
func TestCheckUpstreamMemoizesByCloneURL(t *testing.T) {
	const (
		bdSource   = "https://example.com/mono.git//examples/bd"
		coreSource = "https://example.com/mono.git//internal/core"
	)
	city := newUpstreamFixtureCity(t)
	calls := stubUpstreamNetworkGit(t, func([]string) (string, error) {
		return symrefHeadResponse("refs/heads/main", upstreamMovedHead), nil
	})
	writeTestLockfile(t, city, map[string]LockedPack{
		bdSource:   {Version: "sha:" + upstreamLockedHead, Commit: upstreamLockedHead},
		coreSource: {Version: "sha:" + upstreamLockedHead, Commit: upstreamLockedHead},
	})

	report, err := CheckUpstream(city, map[string]config.Import{
		"pack:bd":   {Source: bdSource, Version: "sha:" + upstreamLockedHead},
		"pack:core": {Source: coreSource, Version: "sha:" + upstreamLockedHead},
	}, nil)
	if err != nil {
		t.Fatalf("CheckUpstream: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("network calls = %v, want 1 for two subpaths of one repository", *calls)
	}
	if n := report.Count(UpstreamBehind); n != 2 {
		t.Fatalf("Count(behind) = %d, want 2", n)
	}
	if len(report.Behind()) != 2 {
		t.Fatalf("len(Behind()) = %d, want 2", len(report.Behind()))
	}
	if report.Checked != 2 {
		t.Fatalf("Checked = %d, want 2", report.Checked)
	}
}

// TestParseSymrefHeadRequiresExactHEADField pins the parse rule directly, so
// the T-1 regression is caught even if the surrounding walk is refactored.
func TestParseSymrefHeadRequiresExactHEADField(t *testing.T) {
	tests := []struct {
		name       string
		out        string
		wantRef    string
		wantCommit string
	}{
		{
			name:       "two-line https response",
			out:        "ref: refs/heads/main\tHEAD\ndead\tHEAD\n",
			wantRef:    "refs/heads/main",
			wantCommit: "dead",
		},
		{
			name:       "remote-tracking refs are not HEAD",
			out:        "ref: refs/remotes/origin/main\trefs/remotes/origin/HEAD\nbeef\trefs/remotes/origin/HEAD\n",
			wantRef:    "",
			wantCommit: "",
		},
		{
			name:       "symref absent",
			out:        "dead\tHEAD\n",
			wantRef:    "",
			wantCommit: "dead",
		},
		{
			name:       "empty response",
			out:        "",
			wantRef:    "",
			wantCommit: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, commit := parseSymrefHead(tt.out)
			if ref != tt.wantRef || commit != tt.wantCommit {
				t.Fatalf("parseSymrefHead() = (%q, %q), want (%q, %q)", ref, commit, tt.wantRef, tt.wantCommit)
			}
		})
	}
}

func newUpstreamFixtureCity(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	stubCachedPackGit(t)
	return city
}

// stubUpstreamNetworkGit swaps the network seam and returns a pointer to the
// recorded argv of every call, so a test can assert what was *not* fetched.
func stubUpstreamNetworkGit(t *testing.T, respond func(args []string) (string, error)) *[]string {
	t.Helper()
	calls := []string{}
	prev := runNetworkGit
	runNetworkGit = func(_, _, _ string, args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		return respond(args)
	}
	t.Cleanup(func() { runNetworkGit = prev })
	return &calls
}

func upstreamFixtureResponse(args []string, symref, tags string) (string, error) {
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "--symref"):
		return symref, nil
	case strings.Contains(joined, "--tags"):
		return tags, nil
	}
	return "", fmt.Errorf("unexpected git invocation: %s", joined)
}

func symrefHeadResponse(ref, commit string) string {
	return fmt.Sprintf("ref: %s\tHEAD\n%s\tHEAD\n", ref, commit)
}

func findUpstreamStatus(t *testing.T, report *UpstreamReport, name string) UpstreamStatus {
	t.Helper()
	for _, status := range report.Statuses {
		if status.Name == name {
			return status
		}
	}
	t.Fatalf("no status for %q in %#v", name, report.Statuses)
	return UpstreamStatus{}
}
