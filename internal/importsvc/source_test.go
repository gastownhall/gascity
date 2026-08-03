package importsvc

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/git"
)

// TestLsRemoteHeadArgsHardened proves the remote HEAD probe is hardened against
// redirect-based SSRF: the SSRF host fence alone is not sufficient because git
// can follow a redirect off the fenced host, so the probe must carry the
// untrusted-remote git config overrides ahead of `ls-remote <url> HEAD`.
func TestLsRemoteHeadArgsHardened(t *testing.T) {
	const url = "https://github.com/example/tools.git"
	args := lsRemoteHeadArgs(url)

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-c http.followRedirects=false") {
		t.Errorf("HEAD probe args do not disable redirect following: %v", args)
	}
	if !strings.Contains(joined, "-c protocol.allow=never") {
		t.Errorf("HEAD probe args do not constrain transports: %v", args)
	}

	// The hardening flags must lead the subcommand, and the tail must be the
	// ls-remote HEAD probe against the given URL.
	hardening := git.UntrustedRemoteGitConfigArgs()
	if len(args) < len(hardening)+3 {
		t.Fatalf("args too short: %v", args)
	}
	tail := args[len(hardening):]
	wantTail := []string{"ls-remote", url, "HEAD"}
	for i, w := range wantTail {
		if tail[i] != w {
			t.Fatalf("tail[%d] = %q, want %q; full args %v", i, tail[i], w, args)
		}
	}
}

// TestDefaultHeadCommitRedactsUserinfo proves the remote-HEAD resolve error
// never echoes a userinfo token embedded in the source. GIT_ALLOW_PROTOCOL=file
// makes git fail the https probe instantly (offline, exit 128) so the resolve
// error path runs without a network round-trip.
func TestDefaultHeadCommitRedactsUserinfo(t *testing.T) {
	t.Setenv("GIT_ALLOW_PROTOCOL", "file")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")

	_, err := defaultHeadCommit("", "https://user:ghp_secret@github.com/example/repo")
	if err == nil {
		t.Fatalf("expected the offline https probe to fail")
	}
	if strings.Contains(err.Error(), "ghp_secret") {
		t.Fatalf("resolve error leaked the userinfo token: %v", err)
	}
}
