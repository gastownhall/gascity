package git

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// captureClone swaps cloneRunner for the test, recording the argv and env of a
// single Clone call and returning them plus a "was it called" flag. runErr is
// what the stub returns to Clone.
func captureClone(t *testing.T, runErr error) (args, env *[]string, called *bool) {
	t.Helper()
	var gotArgs, gotEnv []string
	var invoked bool
	orig := cloneRunner
	cloneRunner = func(_ context.Context, a, e []string) error {
		invoked = true
		gotArgs, gotEnv = a, e
		return runErr
	}
	t.Cleanup(func() { cloneRunner = orig })
	return &gotArgs, &gotEnv, &invoked
}

func TestClone_SchemeAllowlist(t *testing.T) {
	cases := []struct {
		url      string
		allowSSH bool
		want     error // nil = allowed
	}{
		{url: "ext::sh -c 'touch /tmp/pwned'", want: ErrSchemeExt},
		{url: "EXT::sh -c 'x'", want: ErrSchemeExt},
		{url: "fd::17", want: ErrSchemeExt},
		{url: "foo::bar", want: ErrSchemeExt},
		{url: "file:///etc/passwd", want: ErrSchemeFile},
		{url: "file://localhost/repo", want: ErrSchemeFile},
		{url: "FILE:///etc/passwd", want: ErrSchemeFile},
		{url: "/etc/shadow", want: ErrBareLocalPath},
		{url: "./repo", want: ErrBareLocalPath},
		{url: "../repo", want: ErrBareLocalPath},
		{url: "~/repo", want: ErrBareLocalPath},
		{url: "http://github.com/o/r", want: ErrSchemeInsecure},
		{url: "git://github.com/o/r", want: ErrSchemeInsecure},
		{url: "ssh://git@github.com/o/r", allowSSH: false, want: ErrSchemeSSHNotEnabled},
		{url: "git@github.com:o/r", allowSSH: false, want: ErrSchemeSSHNotEnabled},
		{url: "ssh://git@github.com/o/r", allowSSH: true, want: nil},
		{url: "git@github.com:o/r", allowSSH: true, want: nil},
		{url: "https://github.com/o/r", want: nil},
		{url: "-oProxyCommand=evil", want: ErrBareLocalPath},
		{url: "ftp://example.com/r", want: ErrSchemeUnsupported},
	}
	for _, tc := range cases {
		_, _, called := captureClone(t, nil)
		err := Clone(context.Background(), tc.url, "/tmp/dst", CloneOptions{AllowSSH: tc.allowSSH})
		switch {
		case tc.want == nil && err != nil:
			t.Errorf("Clone(%q, AllowSSH=%v) = %v, want allowed", tc.url, tc.allowSSH, err)
		case tc.want != nil && !errors.Is(err, tc.want):
			t.Errorf("Clone(%q, AllowSSH=%v) = %v, want %v", tc.url, tc.allowSSH, err, tc.want)
		}
		// A rejected scheme must never reach the subprocess.
		if tc.want != nil && *called {
			t.Errorf("Clone(%q): subprocess spawned for a rejected scheme", tc.url)
		}
	}
}

func TestClone_UnparseableURLRedacted(t *testing.T) {
	_, _, called := captureClone(t, nil)
	err := Clone(context.Background(), "https://%zz@h/r", "/tmp/dst", CloneOptions{})
	if !errors.Is(err, ErrUnparseableURL) {
		t.Fatalf("Clone(unparseable) = %v, want ErrUnparseableURL", err)
	}
	if *called {
		t.Error("Clone(unparseable): subprocess spawned")
	}
	if strings.Contains(err.Error(), "%zz@") {
		t.Errorf("Clone(unparseable) error leaked userinfo: %v", err)
	}
}

func TestClone_LeadingDashRejectedAndTerminated(t *testing.T) {
	// A leading-dash URL is rejected by the classifier...
	_, _, called := captureClone(t, nil)
	if err := Clone(context.Background(), "-oProxyCommand=evil", "/tmp/dst", CloneOptions{}); !errors.Is(err, ErrBareLocalPath) {
		t.Fatalf("Clone(leading-dash) = %v, want ErrBareLocalPath", err)
	}
	if *called {
		t.Error("Clone(leading-dash): subprocess spawned")
	}
	// ...and even a valid URL argv carries a "--" terminator before url/dst so a
	// dash-leading string could never be parsed as a clone option.
	args := assembleCloneArgs("https://github.com/o/r", "/tmp/dst", CloneOptions{})
	term := indexOf(args, "--")
	urlIdx := indexOf(args, "https://github.com/o/r")
	if term < 0 || urlIdx < 0 || term >= urlIdx {
		t.Errorf("argv missing '--' terminator before url: %v", args)
	}
}

func TestClone_HardenedArgvGolden(t *testing.T) {
	gotArgs, gotEnv, called := captureClone(t, nil)
	dst := "/tmp/stage"
	url := "https://github.com/o/r"
	if err := Clone(context.Background(), url, dst, CloneOptions{}); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if !*called {
		t.Fatal("Clone did not invoke the runner")
	}
	args := *gotArgs

	// The transport-policy overrides must appear in this relative order.
	inOrder := []string{
		"protocol.allow=never",
		"protocol.https.allow=always",
		"protocol.ext.allow=never",
		"protocol.file.allow=never",
		"http.followRedirects=false",
		"core.hooksPath=/dev/null",
		"clone",
		"--no-recurse-submodules",
		"--",
		url,
		dst,
	}
	if !containsInOrder(args, inOrder) {
		t.Errorf("argv missing required tokens in order.\n got: %v\nwant subsequence: %v", args, inOrder)
	}

	// Anti-regression: the permissive pack-helper flags must NEVER appear.
	for _, banned := range []string{
		"protocol.file.allow=always",
		"protocol.http.allow=always",
		"protocol.ext.allow=always",
		"protocol.git.allow=always",
	} {
		if contains(args, banned) {
			t.Errorf("argv contains forbidden permissive flag %q: %v", banned, args)
		}
	}
	// AllowSSH:false must not enable the ssh transport.
	if contains(args, "protocol.ssh.allow=always") {
		t.Errorf("argv enabled ssh transport with AllowSSH=false: %v", args)
	}
	// RecurseSubmodules:false pins submodule.recurse off.
	if !contains(args, "submodule.recurse=false") {
		t.Errorf("argv missing submodule.recurse=false: %v", args)
	}
	if !contains(args, "core.fsmonitor=false") {
		t.Errorf("argv missing core.fsmonitor=false: %v", args)
	}

	// Env (over HermeticEnv) must carry the prompt/askpass/config pins.
	env := *gotEnv
	for _, want := range []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"SSH_ASKPASS=/bin/false",
		"GIT_LFS_SKIP_SMUDGE=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
	} {
		if !contains(env, want) {
			t.Errorf("env missing %q: %v", want, env)
		}
	}
}

func TestClone_AllowSSHAddsTransport(t *testing.T) {
	gotArgs, _, _ := captureClone(t, nil)
	if err := Clone(context.Background(), "ssh://git@github.com/o/r", "/tmp/dst", CloneOptions{AllowSSH: true}); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if !contains(*gotArgs, "protocol.ssh.allow=always") {
		t.Errorf("AllowSSH=true argv missing protocol.ssh.allow=always: %v", *gotArgs)
	}
}

func TestClone_RecurseSubmodulesOmitsGuards(t *testing.T) {
	gotArgs, _, _ := captureClone(t, nil)
	if err := Clone(context.Background(), "https://github.com/o/r", "/tmp/dst", CloneOptions{RecurseSubmodules: true}); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if contains(*gotArgs, "--no-recurse-submodules") {
		t.Errorf("RecurseSubmodules=true still passed --no-recurse-submodules: %v", *gotArgs)
	}
	if contains(*gotArgs, "submodule.recurse=false") {
		t.Errorf("RecurseSubmodules=true still pinned submodule.recurse=false: %v", *gotArgs)
	}
}

func TestClone_DepthAndBranch(t *testing.T) {
	gotArgs, _, _ := captureClone(t, nil)
	if err := Clone(context.Background(), "https://github.com/o/r", "/tmp/dst", CloneOptions{Depth: 1, Branch: "main"}); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if !containsInOrder(*gotArgs, []string{"--depth", "1"}) {
		t.Errorf("argv missing --depth 1: %v", *gotArgs)
	}
	if !containsInOrder(*gotArgs, []string{"--branch", "main"}) {
		t.Errorf("argv missing --branch main: %v", *gotArgs)
	}
}

func TestClone_CredentialNeverLeaksIntoError(t *testing.T) {
	// git echoes the remote URL in transport failures; simulate that with a
	// runner error that carries the raw token. Clone must scrub it.
	runErr := errors.New("fatal: repository 'https://alice:s3cr3t-tok@github.com/o/r/' not found")
	_, _, _ = captureClone(t, runErr)
	err := Clone(context.Background(), "https://alice:s3cr3t-tok@github.com/o/r", "/tmp/dst", CloneOptions{})
	if err == nil {
		t.Fatal("Clone should surface the runner error")
	}
	out := err.Error()
	if strings.Contains(out, "s3cr3t-tok") {
		t.Errorf("Clone error leaked the credential token: %q", out)
	}
	if !strings.Contains(out, "***") {
		t.Errorf("Clone error was not redacted: %q", out)
	}
}

func TestClone_EmptyDstRejected(t *testing.T) {
	_, _, called := captureClone(t, nil)
	if err := Clone(context.Background(), "https://github.com/o/r", "", CloneOptions{}); err == nil {
		t.Fatal("Clone with empty dst should error")
	}
	if *called {
		t.Error("Clone with empty dst spawned a subprocess")
	}
}

// --- small slice helpers ---

func contains(s []string, want string) bool { return indexOf(s, want) >= 0 }

func indexOf(s []string, want string) int {
	for i, v := range s {
		if v == want {
			return i
		}
	}
	return -1
}

// containsInOrder reports whether want appears as a subsequence of s (same
// relative order, gaps allowed).
func containsInOrder(s, want []string) bool {
	i := 0
	for _, v := range s {
		if i < len(want) && v == want[i] {
			i++
		}
	}
	return i == len(want)
}
