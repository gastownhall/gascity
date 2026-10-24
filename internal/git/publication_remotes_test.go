package git

import (
	"path/filepath"
	"testing"
)

// selfReferentialRemoteRepo builds the shape that cost this city two keystone
// fixes (gas-9sg): a repo carrying a remote whose URL is the repo itself.
// Fetching it mirrors every local head into refs/remotes/<name>/*, so
// publication probes that consult all remote-tracking refs report success while
// nothing has left the host. It returns the repo path.
func selfReferentialRemoteRepo(t *testing.T) string {
	const remoteName = "herdr-src" // the real remote name from the incident
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@test.com")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "commit", "--allow-empty", "-m", "work")
	runGit(t, dir, "remote", "add", remoteName, dir)
	runGit(t, dir, "fetch", remoteName)
	return dir
}

// TestHasUnpushedCommitsResult_TrueWhenOnlyRemoteIsSelfReferential is the
// gas-9sg regression: a commit that exists nowhere but this host must read as
// unpushed. Before the fix, `git log HEAD --not --remotes` counted the
// self-referential remote's mirrored refs and reported the work published.
func TestHasUnpushedCommitsResult_TrueWhenOnlyRemoteIsSelfReferential(t *testing.T) {
	dir := selfReferentialRemoteRepo(t)

	has, err := New(dir).HasUnpushedCommitsResult()
	if err != nil {
		t.Fatalf("HasUnpushedCommitsResult() error = %v, want nil", err)
	}
	if !has {
		t.Error("HasUnpushedCommitsResult() = false for a repo whose only remote is itself, want true: pushing there publishes nothing off-host")
	}
}

// TestHasUnpushedCommitsResult_TrueInWorktreeWhenOnlyRemoteIsSelfReferential
// pins the production shape exactly: agents work in linked worktrees, and the
// self-referential remote's URL is the MAIN repo path, not the worktree path.
// A probe that compares the remote URL against the worktree directory alone
// would miss it and keep reporting the work published.
func TestHasUnpushedCommitsResult_TrueInWorktreeWhenOnlyRemoteIsSelfReferential(t *testing.T) {
	main := selfReferentialRemoteRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt")
	runGit(t, main, "worktree", "add", "-b", "polecat/gas-9sg", wtPath)
	runGit(t, wtPath, "config", "user.email", "test@test.com")
	runGit(t, wtPath, "config", "user.name", "Test")
	runGit(t, wtPath, "commit", "--allow-empty", "-m", "unpublished work")
	runGit(t, wtPath, "push", "herdr-src", "polecat/gas-9sg")

	has, err := New(wtPath).HasUnpushedCommitsResult()
	if err != nil {
		t.Fatalf("HasUnpushedCommitsResult() error = %v, want nil", err)
	}
	if !has {
		t.Error("HasUnpushedCommitsResult() = false in a worktree whose only remote is the main repo itself, want true: this is the shape that let the pruner delete unpublished work")
	}
}

// TestHasUnpushedCommitsResult_FalseWhenPushedToASeparateRepo guards the fix
// from over-reach. A remote living at another path is a real, distinct
// publication target — only a remote resolving to THIS repo is the trap — so
// work pushed there must still read as published.
func TestHasUnpushedCommitsResult_FalseWhenPushedToASeparateRepo(t *testing.T) {
	bare := t.TempDir()
	runGit(t, bare, "init", "--bare")
	clone := t.TempDir()
	runGit(t, clone, "clone", bare, ".")
	runGit(t, clone, "config", "user.email", "test@test.com")
	runGit(t, clone, "config", "user.name", "Test")
	runGit(t, clone, "commit", "--allow-empty", "-m", "work")
	runGit(t, clone, "push", "origin", "HEAD")

	has, err := New(clone).HasUnpushedCommitsResult()
	if err != nil {
		t.Fatalf("HasUnpushedCommitsResult() error = %v, want nil", err)
	}
	if has {
		t.Error("HasUnpushedCommitsResult() = true for work pushed to a separate repo, want false")
	}
}

// TestHasUnpushedCommitsResult_TrueWhenRealRemoteLacksTheCommit proves the
// self-referential remote cannot mask an unpushed commit even when a genuine
// remote is also configured: the answer must come from the real remote alone.
func TestHasUnpushedCommitsResult_TrueWhenRealRemoteLacksTheCommit(t *testing.T) {
	bare := t.TempDir()
	runGit(t, bare, "init", "--bare")
	clone := t.TempDir()
	runGit(t, clone, "clone", bare, ".")
	runGit(t, clone, "config", "user.email", "test@test.com")
	runGit(t, clone, "config", "user.name", "Test")
	runGit(t, clone, "commit", "--allow-empty", "-m", "published")
	runGit(t, clone, "push", "origin", "HEAD")
	runGit(t, clone, "remote", "add", "herdr-src", clone)
	runGit(t, clone, "commit", "--allow-empty", "-m", "only on this host")
	runGit(t, clone, "fetch", "herdr-src")

	has, err := New(clone).HasUnpushedCommitsResult()
	if err != nil {
		t.Fatalf("HasUnpushedCommitsResult() error = %v, want nil", err)
	}
	if !has {
		t.Error("HasUnpushedCommitsResult() = false while the real remote lacks the newest commit, want true")
	}
}

// TestHasUnpushedCommitsResult_TrueWithNoRemotesAtAll pins the fail-safe
// direction: with nothing to publish to, everything is unpublished.
func TestHasUnpushedCommitsResult_TrueWithNoRemotesAtAll(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@test.com")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "commit", "--allow-empty", "-m", "work")

	has, err := New(dir).HasUnpushedCommitsResult()
	if err != nil {
		t.Fatalf("HasUnpushedCommitsResult() error = %v, want nil", err)
	}
	if !has {
		t.Error("HasUnpushedCommitsResult() = false for a repo with no remotes, want true")
	}
}

// TestHasUnpushedCommitsResult_TrueWhenSelfReferentialRemoteIsRelative is the
// end-to-end shape of the relative-URL hole: `git remote add <name> .` stores
// "." verbatim and fetching it mirrors every head into refs/remotes/<name>/*,
// so the probe reports the work published unless "." is resolved against the
// repository. This is what the pruner reads before removing a worktree.
func TestHasUnpushedCommitsResult_TrueWhenSelfReferentialRemoteIsRelative(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@test.com")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "commit", "--allow-empty", "-m", "work")
	runGit(t, dir, "remote", "add", "herdr-src", ".")
	runGit(t, dir, "fetch", "herdr-src")

	has, err := New(dir).HasUnpushedCommitsResult()
	if err != nil {
		t.Fatalf("HasUnpushedCommitsResult() error = %v, want nil", err)
	}
	if !has {
		t.Error("HasUnpushedCommitsResult() = false for a repo whose only remote is itself spelled \".\", want true: the relative URL must resolve against the repo, not the process working directory")
	}
}

// --- PublishableRemotes ---

// TestPublishableRemotesExcludesSelfReferentialRemote pins the classifier the
// probe is built on: a remote pointing at this repo is not a publication
// target, while one pointing elsewhere is.
func TestPublishableRemotesExcludesSelfReferentialRemote(t *testing.T) {
	other := t.TempDir()
	runGit(t, other, "init", "--bare")
	dir := selfReferentialRemoteRepo(t)
	runGit(t, dir, "remote", "add", "origin", other)

	names, err := New(dir).PublishableRemotes()
	if err != nil {
		t.Fatalf("PublishableRemotes() error = %v, want nil", err)
	}
	if len(names) != 1 || names[0] != "origin" {
		t.Errorf("PublishableRemotes() = %v, want [origin]", names)
	}
}

// TestPublishableRemotesKeepsNetworkRemotes proves a URL with a scheme is
// always publishable — it names another host, so no path comparison applies.
func TestPublishableRemotesKeepsNetworkRemotes(t *testing.T) {
	dir := selfReferentialRemoteRepo(t)
	runGit(t, dir, "remote", "add", "ajb", "https://github.com/AJBcoding/gascity.git")

	names, err := New(dir).PublishableRemotes()
	if err != nil {
		t.Fatalf("PublishableRemotes() error = %v, want nil", err)
	}
	if len(names) != 1 || names[0] != "ajb" {
		t.Errorf("PublishableRemotes() = %v, want [ajb]", names)
	}
}

// --- relative remote URLs ---

// TestPublishableRemotesExcludesSelfReferentialDotURL pins the resolution base.
// `git remote add <name> .` stores "." verbatim, and git resolves it against
// the repository's worktree root. Resolving it against the calling process's
// working directory instead lands somewhere else entirely, the identity
// comparison misses, and the remote is promoted to publishable — the exact
// false positive this file exists to remove.
func TestPublishableRemotesExcludesSelfReferentialDotURL(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "remote", "add", "herdr-src", ".")

	names, err := New(dir).PublishableRemotes()
	if err != nil {
		t.Fatalf("PublishableRemotes() error = %v, want nil", err)
	}
	if len(names) != 0 {
		t.Errorf("PublishableRemotes() = %v, want [] for a remote spelled \".\": it resolves to this same repo", names)
	}
}

// TestPublishableRemotesExcludesSelfReferentialParentRelativeURL covers the
// "../<repo>" spelling, which resolves back to this repository from its own
// worktree root exactly as git resolves it.
func TestPublishableRemotesExcludesSelfReferentialParentRelativeURL(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "repo")
	runGit(t, parent, "init", "repo")
	runGit(t, dir, "remote", "add", "herdr-src", "../repo")

	names, err := New(dir).PublishableRemotes()
	if err != nil {
		t.Fatalf("PublishableRemotes() error = %v, want nil", err)
	}
	if len(names) != 0 {
		t.Errorf("PublishableRemotes() = %v, want [] for a remote spelled \"../repo\": it resolves to this same repo", names)
	}
}

// TestPublishableRemotesKeepsDistinctRelativeURL guards the relative-URL fix
// from over-reach: a relative URL naming a *different* repository is still a
// real publication target.
func TestPublishableRemotesKeepsDistinctRelativeURL(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "repo")
	runGit(t, parent, "init", "repo")
	runGit(t, parent, "init", "--bare", "mirror.git")
	runGit(t, dir, "remote", "add", "mirror", "../mirror.git")

	names, err := New(dir).PublishableRemotes()
	if err != nil {
		t.Fatalf("PublishableRemotes() error = %v, want nil", err)
	}
	if len(names) != 1 || names[0] != "mirror" {
		t.Errorf("PublishableRemotes() = %v, want [mirror]: a relative URL naming another repo publishes off-host", names)
	}
}

// TestPublishableRemotesExcludesUnresolvableLocalPath pins the fail-safe
// direction for a local path that cannot be stat'ed. A remote we cannot prove
// is a distinct repository must not be counted as one: pushing to a path that
// does not exist publishes nothing, and this predicate gates worktree removal.
func TestPublishableRemotesExcludesUnresolvableLocalPath(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "remote", "add", "gone", filepath.Join(dir, "no", "such", "mirror"))

	names, err := New(dir).PublishableRemotes()
	if err != nil {
		t.Fatalf("PublishableRemotes() error = %v, want nil", err)
	}
	if len(names) != 0 {
		t.Errorf("PublishableRemotes() = %v, want [] for a path that does not exist: it cannot have received anything", names)
	}
}

// TestPublishableRemotesInWorktreeResolvesRelativeToItsOwnRoot pins the base
// git actually uses in a linked worktree: the *linked* worktree's root, not the
// main repo's. The two differ, so picking the wrong one silently classifies the
// wrong directory.
func TestPublishableRemotesInWorktreeResolvesRelativeToItsOwnRoot(t *testing.T) {
	main := t.TempDir()
	runGit(t, main, "init")
	runGit(t, main, "config", "user.email", "test@test.com")
	runGit(t, main, "config", "user.name", "Test")
	runGit(t, main, "commit", "--allow-empty", "-m", "work")

	wtParent := t.TempDir()
	wtPath := filepath.Join(wtParent, "wt")
	runGit(t, main, "worktree", "add", "-b", "polecat/gas-9sg", wtPath)
	// "../mirror.git" from the linked worktree root — a real, distinct repo that
	// exists only next to the worktree, never next to the main repo.
	runGit(t, wtParent, "init", "--bare", "mirror.git")
	runGit(t, wtPath, "remote", "add", "mirror", "../mirror.git")

	names, err := New(wtPath).PublishableRemotes()
	if err != nil {
		t.Fatalf("PublishableRemotes() error = %v, want nil", err)
	}
	if len(names) != 1 || names[0] != "mirror" {
		t.Errorf("PublishableRemotes() = %v, want [mirror]: git resolves a relative remote URL against the linked worktree's own root", names)
	}
}

// --- loopback URLs ---

// TestPublishableRemotesExcludesSelfReferentialLoopbackSSHURL covers the
// containerized shape: an ssh:// URL whose host is the loopback interface names
// a path on THIS machine, so it can be self-referential like any local path.
// Classifying every scheme-bearing URL as off-host misses it.
func TestPublishableRemotesExcludesSelfReferentialLoopbackSSHURL(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "[::1]"} {
		t.Run(host, func(t *testing.T) {
			dir := t.TempDir()
			runGit(t, dir, "init")
			runGit(t, dir, "remote", "add", "herdr-src", "ssh://"+host+dir)

			names, err := New(dir).PublishableRemotes()
			if err != nil {
				t.Fatalf("PublishableRemotes() error = %v, want nil", err)
			}
			if len(names) != 0 {
				t.Errorf("PublishableRemotes() = %v, want [] for ssh://%s pointing at this repo", names, host)
			}
		})
	}
}

// TestPublishableRemotesExcludesSelfReferentialLoopbackSCPURL covers the
// scp-style spelling of the same trap: "user@localhost:/path".
func TestPublishableRemotesExcludesSelfReferentialLoopbackSCPURL(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "remote", "add", "herdr-src", "git@localhost:"+dir)

	names, err := New(dir).PublishableRemotes()
	if err != nil {
		t.Fatalf("PublishableRemotes() error = %v, want nil", err)
	}
	if len(names) != 0 {
		t.Errorf("PublishableRemotes() = %v, want [] for git@localhost pointing at this repo", names)
	}
}

// TestPublishableRemotesKeepsLoopbackURLToADistinctRepo guards the loopback fix
// from over-reach: a loopback URL naming another repository on this host is
// still a distinct repository, exactly like a plain local path is.
func TestPublishableRemotesKeepsLoopbackURLToADistinctRepo(t *testing.T) {
	other := t.TempDir()
	runGit(t, other, "init", "--bare")
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "remote", "add", "mirror", "ssh://localhost"+other)

	names, err := New(dir).PublishableRemotes()
	if err != nil {
		t.Fatalf("PublishableRemotes() error = %v, want nil", err)
	}
	if len(names) != 1 || names[0] != "mirror" {
		t.Errorf("PublishableRemotes() = %v, want [mirror]: a loopback URL to another repo is a distinct target", names)
	}
}

// TestPublishableRemotesKeepsNonLoopbackSSHURL proves the host check is what
// distinguishes the loopback case — a named host is still off-host.
func TestPublishableRemotesKeepsNonLoopbackSSHURL(t *testing.T) {
	dir := selfReferentialRemoteRepo(t)
	runGit(t, dir, "remote", "add", "build", "ssh://buildbox.internal"+dir)

	names, err := New(dir).PublishableRemotes()
	if err != nil {
		t.Fatalf("PublishableRemotes() error = %v, want nil", err)
	}
	if len(names) != 1 || names[0] != "build" {
		t.Errorf("PublishableRemotes() = %v, want [build]: a named host is another machine even at the same path", names)
	}
}

// TestPublishableRemotesExcludesSelfReferentialFileURLWithLocalhostHost covers
// the "file://localhost/path" spelling, whose authority is a host, not the
// first path segment.
func TestPublishableRemotesExcludesSelfReferentialFileURLWithLocalhostHost(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "remote", "add", "herdr-src", "file://localhost"+dir)

	names, err := New(dir).PublishableRemotes()
	if err != nil {
		t.Fatalf("PublishableRemotes() error = %v, want nil", err)
	}
	if len(names) != 0 {
		t.Errorf("PublishableRemotes() = %v, want [] for file://localhost pointing at this repo", names)
	}
}
