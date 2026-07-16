package runtime

import (
	"context"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func withZeroDialogTimings(t *testing.T) {
	t.Helper()
	oldPollInterval := dialogPollInterval
	oldPollTimeout := dialogPollTimeout
	oldAcceptDelay := startupDialogAcceptDelay
	oldConfirmDelay := bypassDialogConfirmDelay
	dialogPollInterval = 0
	dialogPollTimeout = 0
	startupDialogAcceptDelay = 0
	bypassDialogConfirmDelay = 0
	t.Cleanup(func() {
		dialogPollInterval = oldPollInterval
		dialogPollTimeout = oldPollTimeout
		startupDialogAcceptDelay = oldAcceptDelay
		bypassDialogConfirmDelay = oldConfirmDelay
	})
}

func TestContainsWorkspaceTrustDialog(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "claude quick safety check",
			content: "Quick safety check\nYes, I trust this folder",
			want:    true,
		},
		{
			name:    "claude trust this folder",
			content: "Do you trust this folder?",
			want:    true,
		},
		{
			name:    "codex trust dialog",
			content: "> Do you trust the contents of this directory?",
			want:    true,
		},
		{
			name:    "gemini trust dialog",
			content: "Do you trust the files in this folder?\n1. Trust folder",
			want:    true,
		},
		{
			name:    "normal prompt text",
			content: "> waiting for input",
			want:    false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := containsWorkspaceTrustDialog(tt.content); got != tt.want {
				t.Fatalf("containsWorkspaceTrustDialog(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

func TestAcceptStartupDialogsAcceptsCodexTrustDialog(t *testing.T) {
	withZeroDialogTimings(t)
	// Override timeout to allow at least one poll iteration.
	dialogPollTimeout = time.Second

	var sent []string
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) {
			if len(sent) == 0 {
				return "Do you trust the contents of this directory?", nil
			}
			return "user@host $", nil
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs() error = %v", err)
	}
	if !reflect.DeepEqual(sent, []string{"Enter"}) {
		t.Fatalf("sent keys = %v, want [Enter]", sent)
	}
}

func TestAcceptStartupDialogsAcceptsGeminiTrustDialog(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var sent []string
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) {
			if len(sent) == 0 {
				return "Do you trust the files in this folder?\n● 1. Trust folder (city)\n  2. Trust parent folder\n  3. Don't trust", nil
			}
			return "Type your message or @path/to/file", nil
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs() error = %v", err)
	}
	if !reflect.DeepEqual(sent, []string{"Enter"}) {
		t.Fatalf("sent keys = %v, want [Enter]", sent)
	}
}

func TestAcceptStartupDialogsSelectsClaudeResumeAsIs(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var sent []string
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) {
			if len(sent) == 0 {
				return strings.Join([]string{
					"This session is 1h 55m old and 212.7k tokens.",
					"",
					"❯ 1. Resume from summary (recommended)",
					"  2. Resume full session as-is",
					"  3. Don't ask me again",
					"",
					"Enter to confirm · Esc to cancel",
				}, "\n"), nil
			}
			return "❯ ", nil
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs() error = %v", err)
	}
	if !reflect.DeepEqual(sent, []string{"Down", "Enter"}) {
		t.Fatalf("sent keys = %v, want [Down Enter]", sent)
	}
}

func TestAcceptStartupDialogsFromStreamSelectsClaudeResumeAsIs(t *testing.T) {
	withZeroDialogTimings(t)

	snapshots := make(chan string, 2)
	snapshots <- strings.Join([]string{
		"This session is 1h 55m old and 212.7k tokens.",
		"",
		"❯ 1. Resume from summary (recommended)",
		"  2. Resume full session as-is",
		"  3. Don't ask me again",
		"",
		"Enter to confirm · Esc to cancel",
	}, "\n")
	snapshots <- "❯ "
	close(snapshots)

	var sent []string
	err := AcceptStartupDialogsFromStream(
		context.Background(),
		time.Second,
		snapshots,
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogsFromStream() error = %v", err)
	}
	if !reflect.DeepEqual(sent, []string{"Down", "Enter"}) {
		t.Fatalf("sent keys = %v, want [Down Enter]", sent)
	}
}

func TestAcceptStartupDialogsPeeksDeepEnoughForLateTrustDialog(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var sent []string
	err := AcceptStartupDialogs(
		context.Background(),
		func(lines int) (string, error) {
			if lines < 100 {
				return "› Implement {feature}", nil
			}
			return "Do you trust the contents of this directory?\n› Implement {feature}", nil
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs() error = %v", err)
	}
	if !reflect.DeepEqual(sent, []string{"Enter"}) {
		t.Fatalf("sent keys = %v, want [Enter]", sent)
	}
}

func TestAcceptStartupDialogsSkipsCodexUpdateDialog(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var sent []string
	err := AcceptStartupDialogs(
		context.Background(),
		func(lines int) (string, error) {
			if lines < 100 {
				return "loading...", nil
			}
			return "✨ Update available! 0.124.0 -> 0.125.0\n" +
				"› 1. Update now (runs `bun install -g @openai/codex`)\n" +
				"  2. Skip\n" +
				"  3. Skip until next version\n" +
				"Press enter to continue", nil
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs returned error: %v", err)
	}
	if got, want := strings.Join(sent, ","), "Down,Enter"; got != want {
		t.Fatalf("sent keys = %q, want %q", got, want)
	}
}

func TestAcceptStartupDialogsSkipsUpdateThenHandlesTrustDialog(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var sent []string
	staleUpdateReturned := false
	err := AcceptStartupDialogs(
		context.Background(),
		func(lines int) (string, error) {
			if lines < 100 {
				return "loading...", nil
			}
			switch {
			case len(sent) < 2:
				return codexUpdateDialogFixture(), nil
			case !staleUpdateReturned:
				staleUpdateReturned = true
				return codexUpdateDialogFixture(), nil
			case len(sent) == 2:
				return "Do you trust the contents of this directory?", nil
			default:
				return "› Implement {feature}", nil
			}
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs returned error: %v", err)
	}
	if got, want := strings.Join(sent, ","), "Down,Enter,Enter"; got != want {
		t.Fatalf("sent keys = %q, want %q", got, want)
	}
}

func TestAcceptStartupDialogsTrustsCodexHookReviewDialog(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var sent []string
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) {
			if len(sent) == 0 {
				return codexHookReviewDialogFixture(), nil
			}
			return "› Implement {feature}", nil
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs returned error: %v", err)
	}
	if got, want := strings.Join(sent, ","), "Down,Enter"; got != want {
		t.Fatalf("sent keys = %q, want %q", got, want)
	}
}

func TestAcceptStartupDialogsHandlesTrustThenCodexHookReview(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var sent []string
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) {
			switch len(sent) {
			case 0:
				return "Do you trust the contents of this directory?", nil
			case 1:
				return codexHookReviewDialogFixture(), nil
			default:
				return "› Implement {feature}", nil
			}
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs returned error: %v", err)
	}
	if got, want := strings.Join(sent, ","), "Enter,Down,Enter"; got != want {
		t.Fatalf("sent keys = %q, want %q", got, want)
	}
}

func TestContainsMCPTrustDialog(t *testing.T) {
	t.Parallel()

	if !containsMCPTrustDialog(mcpTrustDialogFixture()) {
		t.Error("containsMCPTrustDialog should match the MCP trust modal")
	}
	if containsMCPTrustDialog("Do you trust the contents of this directory?") {
		t.Error("containsMCPTrustDialog should not match the workspace trust dialog")
	}
	if containsMCPTrustDialog("› Implement {feature}") {
		t.Error("containsMCPTrustDialog should not match a ready prompt")
	}
}

func TestAcceptStartupDialogsAcceptsMCPTrustDialog(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var sent []string
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) {
			if len(sent) == 0 {
				return mcpTrustDialogFixture(), nil
			}
			return "› Implement {feature}", nil
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs returned error: %v", err)
	}
	if got, want := strings.Join(sent, ","), "Down,Enter"; got != want {
		t.Fatalf("sent keys = %q, want %q", got, want)
	}
}

func TestAcceptStartupDialogsHandlesTrustThenMCPTrust(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var sent []string
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) {
			switch len(sent) {
			case 0:
				return "Do you trust the contents of this directory?", nil
			case 1:
				return mcpTrustDialogFixture(), nil
			default:
				return "› Implement {feature}", nil
			}
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs returned error: %v", err)
	}
	if got, want := strings.Join(sent, ","), "Enter,Down,Enter"; got != want {
		t.Fatalf("sent keys = %q, want %q", got, want)
	}
}

func TestAcceptStartupDialogsFromStreamAcceptsMCPTrustDialog(t *testing.T) {
	var sent []string
	snapshots := make(chan string, 2)
	snapshots <- mcpTrustDialogFixture()
	snapshots <- "› Implement {feature}"
	close(snapshots)

	err := AcceptStartupDialogsFromStream(
		context.Background(),
		time.Second,
		snapshots,
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogsFromStream() error = %v", err)
	}
	if got, want := strings.Join(sent, ","), "Down,Enter"; got != want {
		t.Fatalf("sent keys = %q, want %q", got, want)
	}
}

func TestContainsExternalImportsDialog(t *testing.T) {
	t.Parallel()

	if !containsExternalImportsDialog(externalImportsDialogFixture()) {
		t.Error("containsExternalImportsDialog should match the external imports modal")
	}
	if containsExternalImportsDialog(mcpTrustDialogFixture()) {
		t.Error("containsExternalImportsDialog should not match the MCP trust dialog")
	}
	if containsExternalImportsDialog("Do you trust the contents of this directory?") {
		t.Error("containsExternalImportsDialog should not match the workspace trust dialog")
	}
	if containsExternalImportsDialog("› Implement {feature}") {
		t.Error("containsExternalImportsDialog should not match a ready prompt")
	}
}

func TestParseExternalImportPaths(t *testing.T) {
	t.Parallel()

	got := parseExternalImportPaths(externalImportsDialogFixture())
	want := []string{"/data/projects/gascity/AGENTS.md"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseExternalImportPaths = %v, want %v", got, want)
	}

	if got := parseExternalImportPaths(mcpTrustDialogFixture()); len(got) != 0 {
		t.Fatalf("parseExternalImportPaths on unrelated modal = %v, want none", got)
	}

	multi := "External imports:\n" +
		"  /data/projects/gascity/AGENTS.md\n" +
		"  /data/projects/gascity/docs/CLAUDE.md\n" +
		"Important: only use files you trust\n"
	if got, want := parseExternalImportPaths(multi),
		[]string{"/data/projects/gascity/AGENTS.md", "/data/projects/gascity/docs/CLAUDE.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("parseExternalImportPaths(multi) = %v, want %v", got, want)
	}
}

func TestPathWithinTrustRoot(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		importPath string
		trustRoot  string
		want       bool
	}{
		{"repo-root file inside root", "/data/projects/gascity/AGENTS.md", "/data/projects/gascity", true},
		{"nested file inside root", "/data/projects/gascity/docs/CLAUDE.md", "/data/projects/gascity", true},
		{"import equals root", "/data/projects/gascity", "/data/projects/gascity", true},
		{"parent-directory file is not trusted", "/data/projects/secrets.md", "/data/projects/gascity", false},
		{"grandparent file is not trusted", "/data/secrets.md", "/data/projects/gascity", false},
		{"sibling prefix is not trusted", "/data/projects/gascity-evil/CLAUDE.md", "/data/projects/gascity", false},
		{"unrelated third-party path", "/home/attacker/repo/CLAUDE.md", "/data/projects/gascity", false},
		{"dot-dot traversal is cleaned then rejected", "/data/projects/gascity/../secrets.md", "/data/projects/gascity", false},
		{"relative import path rejected", "relative/CLAUDE.md", "/data/projects/gascity", false},
		{"tilde import path rejected", "~/secrets/CLAUDE.md", "/data/projects/gascity", false},
		{"empty root rejects", "/data/projects/gascity/AGENTS.md", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pathWithinTrustRoot(tc.importPath, tc.trustRoot); got != tc.want {
				t.Fatalf("pathWithinTrustRoot(%q, %q) = %v, want %v", tc.importPath, tc.trustRoot, got, tc.want)
			}
		})
	}
}

func TestExternalImportsTrusted(t *testing.T) {
	t.Parallel()

	if !externalImportsTrusted(externalImportsDialogFixture(), trustedImportRootFixture) {
		t.Error("first-party import within an enclosing repo should be trusted")
	}
	if externalImportsTrusted(externalImportsDialogFixture(), "/tmp/other/wt") {
		t.Error("import outside the trust root must not be trusted")
	}
	if externalImportsTrusted(externalImportsDialogFixture(), "") {
		t.Error("empty trust root must trust nothing")
	}
	// A modal with no parseable "External imports:" list is unverifiable and
	// must fail closed even when a trust root is supplied.
	if externalImportsTrusted("Allow external CLAUDE.md ... allow external imports", trustedImportRootFixture) {
		t.Error("unparseable import list must not be trusted")
	}
	// If any listed import escapes the trust root, the whole modal is untrusted.
	mixed := "Allow external CLAUDE.md file imports?\nallow external imports\n" +
		"External imports:\n" +
		"  /data/projects/gascity/AGENTS.md\n" +
		"  /home/attacker/evil.md\n"
	if externalImportsTrusted(mixed, trustedImportRootFixture) {
		t.Error("a single untrusted import must make the modal untrusted")
	}
	// An in-root import that points at repository metadata or runtime state
	// (.git, .gc) is not a first-party instruction file and must fail closed,
	// even though it resolves inside the trust root.
	for _, runtimePath := range []string{
		"/data/projects/gascity/.git/config",
		"/data/projects/gascity/.gc/worktrees/other/CLAUDE.md",
	} {
		modal := "Allow external CLAUDE.md file imports?\nallow external imports\n" +
			"External imports:\n  " + runtimePath + "\n"
		if externalImportsTrusted(modal, trustedImportRootFixture) {
			t.Errorf("import of repository runtime path %q must not be trusted", runtimePath)
		}
	}
}

func TestImportPathFirstParty(t *testing.T) {
	t.Parallel()

	const root = "/data/projects/gascity"
	cases := []struct {
		name       string
		importPath string
		want       bool
	}{
		{"repo-root AGENTS.md is first-party", root + "/AGENTS.md", true},
		{"repo-root CLAUDE.md is first-party", root + "/CLAUDE.md", true},
		{"nested instruction file is first-party", root + "/docs/CLAUDE.md", true},
		{"git config is refused", root + "/.git/config", false},
		{"nested git metadata is refused", root + "/.git/hooks/pre-commit", false},
		{"gc runtime state is refused", root + "/.gc/worktrees/other/CLAUDE.md", false},
		{"hidden tool cache is refused", root + "/.claude/settings.json", false},
		{"hidden file at root is refused", root + "/.env", false},
		{"path outside the root is refused", "/data/projects/secrets.md", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := importPathFirstParty(tc.importPath, root); got != tc.want {
				t.Fatalf("importPathFirstParty(%q, %q) = %v, want %v", tc.importPath, root, got, tc.want)
			}
		})
	}
}

// trustedImportRootFixture is the repository root that contains the external
// import in externalImportsDialogFixture (/data/projects/gascity/AGENTS.md), so
// the import is first-party and auto-acceptance is allowed.
const trustedImportRootFixture = "/data/projects/gascity"

func TestAcceptStartupDialogsAcceptsExternalImportsDialog(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var sent []string
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) {
			if len(sent) == 0 {
				return externalImportsDialogFixture(), nil
			}
			return "› Implement {feature}", nil
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
		WithTrustedImportRoot(trustedImportRootFixture),
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs returned error: %v", err)
	}
	if got, want := strings.Join(sent, ","), "Enter"; got != want {
		t.Fatalf("sent keys = %q, want %q", got, want)
	}
}

func TestAcceptStartupDialogsLeavesUntrustedExternalImportsDialog(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = 200 * time.Millisecond

	var sent []string
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) { return externalImportsDialogFixture(), nil },
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
		// A third-party worktree unrelated to the imported path: the modal must
		// be left unaccepted rather than pressing Enter on external files.
		WithTrustedImportRoot("/tmp/some-third-party-repo/wt"),
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs returned error: %v", err)
	}
	if len(sent) != 0 {
		t.Fatalf("sent keys = %v, want none (untrusted external import must not be accepted)", sent)
	}
}

func TestAcceptStartupDialogsLeavesExternalImportsWithoutTrustRoot(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = 200 * time.Millisecond

	var sent []string
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) { return externalImportsDialogFixture(), nil },
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs returned error: %v", err)
	}
	if len(sent) != 0 {
		t.Fatalf("sent keys = %v, want none (no trust root configured)", sent)
	}
}

func TestAcceptStartupDialogsHandlesTrustThenExternalImportsThenMCP(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var sent []string
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) {
			switch len(sent) {
			case 0:
				return "Do you trust the contents of this directory?", nil
			case 1:
				return externalImportsDialogFixture(), nil
			case 2:
				return mcpTrustDialogFixture(), nil
			default:
				return "› Implement {feature}", nil
			}
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
		WithTrustedImportRoot(trustedImportRootFixture),
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs returned error: %v", err)
	}
	if got, want := strings.Join(sent, ","), "Enter,Enter,Down,Enter"; got != want {
		t.Fatalf("sent keys = %q, want %q", got, want)
	}
}

func TestAcceptStartupDialogsFromStreamAcceptsExternalImportsDialog(t *testing.T) {
	var sent []string
	snapshots := make(chan string, 2)
	snapshots <- externalImportsDialogFixture()
	snapshots <- "› Implement {feature}"
	close(snapshots)

	err := AcceptStartupDialogsFromStream(
		context.Background(),
		time.Second,
		snapshots,
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
		WithTrustedImportRoot(trustedImportRootFixture),
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogsFromStream() error = %v", err)
	}
	if got, want := strings.Join(sent, ","), "Enter"; got != want {
		t.Fatalf("sent keys = %q, want %q", got, want)
	}
}

func TestAcceptStartupDialogsFromStreamLeavesUntrustedExternalImportsDialog(t *testing.T) {
	var sent []string
	snapshots := make(chan string, 2)
	snapshots <- externalImportsDialogFixture()
	snapshots <- "› Implement {feature}"
	close(snapshots)

	err := AcceptStartupDialogsFromStream(
		context.Background(),
		200*time.Millisecond,
		snapshots,
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
		// A third-party worktree unrelated to the imported path.
		WithTrustedImportRoot("/tmp/some-third-party-repo/wt"),
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogsFromStream() error = %v", err)
	}
	if len(sent) != 0 {
		t.Fatalf("sent keys = %v, want none (untrusted external import must not be accepted)", sent)
	}
}

// TestAcceptStartupDialogsFromStreamHandlesTrustThenExternalImportsThenMCP
// mirrors the poll-path ordering test on the stream path: workspace-trust yields
// to the external-imports phase (via post-trust snapshots) and then to MCP trust.
// It guards the containsExternalImportsDialog entry in
// containsPostTrustStartupDialog so a future edit cannot silently drop the
// post-trust stream handoff into the new phase.
func TestAcceptStartupDialogsFromStreamHandlesTrustThenExternalImportsThenMCP(t *testing.T) {
	var sent []string
	snapshots := make(chan string, 4)
	snapshots <- "Do you trust the contents of this directory?"
	snapshots <- externalImportsDialogFixture()
	snapshots <- mcpTrustDialogFixture()
	snapshots <- "› Implement {feature}"
	close(snapshots)

	err := AcceptStartupDialogsFromStream(
		context.Background(),
		time.Second,
		snapshots,
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
		WithTrustedImportRoot(trustedImportRootFixture),
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogsFromStream() error = %v", err)
	}
	if got, want := strings.Join(sent, ","), "Enter,Enter,Down,Enter"; got != want {
		t.Fatalf("sent keys = %q, want %q", got, want)
	}
}

func TestAcceptStartupDialogsFromStreamSkipsCodexUpdateDialog(t *testing.T) {
	var sent []string
	snapshots := make(chan string, 2)
	snapshots <- codexUpdateDialogFixture()
	snapshots <- "› Implement {feature}"
	close(snapshots)

	err := AcceptStartupDialogsFromStream(
		context.Background(),
		time.Second,
		snapshots,
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogsFromStream() error = %v", err)
	}
	if got, want := strings.Join(sent, ","), "Down,Enter"; got != want {
		t.Fatalf("sent keys = %q, want %q", got, want)
	}
}

func TestAcceptStartupDialogsFromStreamTrustsCodexHookReviewDialog(t *testing.T) {
	var sent []string
	snapshots := make(chan string, 2)
	snapshots <- codexHookReviewDialogFixture()
	snapshots <- "› Implement {feature}"
	close(snapshots)

	err := AcceptStartupDialogsFromStream(
		context.Background(),
		time.Second,
		snapshots,
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogsFromStream() error = %v", err)
	}
	if got, want := strings.Join(sent, ","), "Down,Enter"; got != want {
		t.Fatalf("sent keys = %q, want %q", got, want)
	}
}

func TestAcceptStartupDialogsAcceptsBypassPermissionsWarning(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var sent []string
	call := 0
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) {
			call++
			if call <= 2 {
				// First two peeks: no trust dialog, no bypass. Then bypass appears.
				return "normal startup output", nil
			}
			return "Bypass Permissions mode", nil
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs() error = %v", err)
	}
	if !reflect.DeepEqual(sent, []string{"Down", "Enter"}) {
		t.Fatalf("sent keys = %v, want [Down Enter]", sent)
	}
}

func TestAcceptStartupDialogsAcceptsCustomAPIKeyDialog(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var sent []string
	call := 0
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) {
			call++
			if call <= 2 {
				return "normal startup output", nil
			}
			return "Detected a custom API key in your environment\nDo you want to use this API key?\n1. Yes\n2. No (recommended)", nil
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs() error = %v", err)
	}
	if !reflect.DeepEqual(sent, []string{"Up", "Enter"}) {
		t.Fatalf("sent keys = %v, want [Up Enter]", sent)
	}
}

func TestAcceptStartupDialogsFromStreamAcceptsTrustDialog(t *testing.T) {
	var sent []string
	snapshots := make(chan string, 2)
	snapshots <- "Do you trust the contents of this directory?"
	snapshots <- "user@host $"
	close(snapshots)

	err := AcceptStartupDialogsFromStream(
		context.Background(),
		time.Second,
		snapshots,
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogsFromStream() error = %v", err)
	}
	if !reflect.DeepEqual(sent, []string{"Enter"}) {
		t.Fatalf("sent keys = %v, want [Enter]", sent)
	}
}

func TestAcceptWorkspaceTrustDialogFromStreamPreservesEarlierSnapshots(t *testing.T) {
	stream := &replayableSnapshotStream{update: make(chan struct{})}
	stream.publish("Do you trust the contents of this directory?")
	stream.publish("user@host $")
	stream.finish()

	var sent []string
	_, err := acceptWorkspaceTrustDialogFromStream(
		context.Background(),
		time.Second,
		newReplayableSnapshotCursorFromStream(stream),
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("acceptWorkspaceTrustDialogFromStream() error = %v", err)
	}
	if !reflect.DeepEqual(sent, []string{"Enter"}) {
		t.Fatalf("sent keys = %v, want [Enter]", sent)
	}
}

func TestAcceptStartupDialogsFromStreamPrefersLaterDialogOverEarlierPrompt(t *testing.T) {
	var sent []string
	snapshots := make(chan string, 2)
	snapshots <- "user@host $"
	snapshots <- "Bypass Permissions mode"
	close(snapshots)

	err := AcceptStartupDialogsFromStream(
		context.Background(),
		time.Second,
		snapshots,
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogsFromStream() error = %v", err)
	}
	if !reflect.DeepEqual(sent, []string{"Down", "Enter"}) {
		t.Fatalf("sent keys = %v, want [Down Enter]", sent)
	}
}

func TestAcceptStartupDialogsFromStreamWaitsBrieflyForDelayedDialogAfterPrompt(t *testing.T) {
	oldGrace := startupDialogStreamReadyGrace
	startupDialogStreamReadyGrace = 75 * time.Millisecond
	t.Cleanup(func() {
		startupDialogStreamReadyGrace = oldGrace
	})

	var sent []string
	snapshots := make(chan string, 1)
	snapshots <- "user@host $"
	go func() {
		time.Sleep(20 * time.Millisecond)
		snapshots <- "Bypass Permissions mode"
		close(snapshots)
	}()

	err := AcceptStartupDialogsFromStream(
		context.Background(),
		time.Second,
		snapshots,
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogsFromStream() error = %v", err)
	}
	if !reflect.DeepEqual(sent, []string{"Down", "Enter"}) {
		t.Fatalf("sent keys = %v, want [Down Enter]", sent)
	}
}

func TestAcceptBypassPermissionsWarningFromStreamSendsKeysSeparately(t *testing.T) {
	oldDelay := bypassDialogConfirmDelay
	bypassDialogConfirmDelay = 10 * time.Millisecond
	t.Cleanup(func() {
		bypassDialogConfirmDelay = oldDelay
	})

	stream := &replayableSnapshotStream{update: make(chan struct{})}
	stream.publish("Bypass Permissions mode")
	stream.finish()

	var calls []string
	var callTimes []time.Time
	_, err := acceptBypassPermissionsWarningFromStream(
		context.Background(),
		time.Second,
		newReplayableSnapshotCursorFromStream(stream),
		func(keys ...string) error {
			calls = append(calls, strings.Join(keys, ","))
			callTimes = append(callTimes, time.Now())
			return nil
		},
	)
	if err != nil {
		t.Fatalf("acceptBypassPermissionsWarningFromStream() error = %v", err)
	}
	if !reflect.DeepEqual(calls, []string{"Down", "Enter"}) {
		t.Fatalf("calls = %v, want [Down Enter]", calls)
	}
	if len(callTimes) != 2 || callTimes[1].Sub(callTimes[0]) < 10*time.Millisecond {
		t.Fatalf("callTimes gap = %v, want >= 10ms", callTimes[1].Sub(callTimes[0]))
	}
}

func TestAcceptStartupDialogsFromStreamReplaysBypassDialogAcrossPhases(t *testing.T) {
	var sent []string
	snapshots := make(chan string, 1)
	snapshots <- "Bypass Permissions mode"
	close(snapshots)

	err := AcceptStartupDialogsFromStream(
		context.Background(),
		time.Second,
		snapshots,
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogsFromStream() error = %v", err)
	}
	if !reflect.DeepEqual(sent, []string{"Down", "Enter"}) {
		t.Fatalf("sent keys = %v, want [Down Enter]", sent)
	}
}

func TestAcceptStartupDialogsFromStreamReplaysCustomAPIKeyDialogAcrossPhases(t *testing.T) {
	var sent []string
	snapshots := make(chan string, 1)
	snapshots <- "Detected a custom API key in your environment\nDo you want to use this API key?"
	close(snapshots)

	err := AcceptStartupDialogsFromStream(
		context.Background(),
		time.Second,
		snapshots,
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogsFromStream() error = %v", err)
	}
	if !reflect.DeepEqual(sent, []string{"Up", "Enter"}) {
		t.Fatalf("sent keys = %v, want [Up Enter]", sent)
	}
}

func TestAcceptStartupDialogsFromStreamReplaysRateLimitDialogAcrossPhases(t *testing.T) {
	var sent []string
	snapshots := make(chan string, 1)
	snapshots <- "Usage limit reached"
	close(snapshots)

	err := AcceptStartupDialogsFromStream(
		context.Background(),
		time.Second,
		snapshots,
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogsFromStream() error = %v", err)
	}
	if !reflect.DeepEqual(sent, []string{"Down", "Enter"}) {
		t.Fatalf("sent keys = %v, want [Down Enter]", sent)
	}
}

func TestAcceptStartupDialogsFromStreamTimesOutDespiteContinuousIrrelevantSnapshots(t *testing.T) {
	stream := &replayableSnapshotStream{update: make(chan struct{})}
	donePublishing := make(chan struct{})
	go func() {
		defer close(donePublishing)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for i := 0; i < 50; i++ {
			stream.publish("still booting")
			<-ticker.C
		}
		stream.finish()
	}()

	start := time.Now()
	_, err := acceptWorkspaceTrustDialogFromStream(
		context.Background(),
		30*time.Millisecond,
		newReplayableSnapshotCursorFromStream(stream),
		func(_ ...string) error { return nil },
	)
	if err != nil {
		t.Fatalf("acceptWorkspaceTrustDialogFromStream() error = %v, want nil timeout exit", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("acceptWorkspaceTrustDialogFromStream() took %s, want timeout-bounded exit", elapsed)
	}
	<-donePublishing
}

func TestAcceptStartupDialogsFromStreamWithStatusReturnsFalseAfterIrrelevantSnapshots(t *testing.T) {
	observed, err := AcceptStartupDialogsFromStreamWithStatus(
		context.Background(),
		30*time.Millisecond,
		func() <-chan string {
			snapshots := make(chan string, 1)
			snapshots <- "starting up"
			close(snapshots)
			return snapshots
		}(),
		func(_ ...string) error { return nil },
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogsFromStreamWithStatus() error = %v", err)
	}
	if observed {
		t.Fatal("AcceptStartupDialogsFromStreamWithStatus() observed = true, want false")
	}
}

func TestContainsPromptIndicator(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{name: "dollar prompt", content: "user@host $", want: true},
		{name: "hash prompt", content: "root@host #", want: true},
		{name: "percent prompt", content: "zsh %", want: true},
		{name: "angle prompt", content: "claude >", want: true},
		{name: "powerline prompt", content: "dir \u276f", want: true},
		{name: "claude nbsp prompt", content: "❯\u00a0", want: true},
		{name: "codex prompt", content: "›", want: true},
		{name: "codex prompt with nbsp", content: "›\u00a0", want: true},
		{name: "codex prompt with placeholder", content: "› Improve documentation in @filename", want: true},
		{name: "claude prompt with text", content: "❯ run tests", want: true},
		{name: "boxed grok prompt", content: "│ ❯ ", want: true},
		{name: "boxed grok prompt with text", content: "│ ❯ start working", want: true},
		// gemini renders an ASCII "> " prompt followed by placeholder text
		// (gastownhall/gascity#2874); the dialog poller must see it as ready so
		// it stops burning the 8s-per-handler budget and the start deadline.
		{name: "gemini ascii prompt with placeholder", content: "> Type your message or @path/to/file", want: true},
		{name: "boxed gemini ascii prompt", content: "│ > Type your message or @path/to/file              │", want: true},
		{name: "codex numbered menu row", content: "› 1. Update now (runs `bun install -g @openai/codex`)", want: false},
		{name: "ascii numbered menu row", content: "> 1. Update now", want: false},
		{name: "empty content", content: "", want: false},
		{name: "no prompt", content: "loading...", want: false},
		{name: "blank lines only", content: "\n\n", want: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := containsPromptIndicator(tt.content); got != tt.want {
				t.Fatalf("containsPromptIndicator(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

func codexUpdateDialogFixture() string {
	return "✨ Update available! 0.124.0 -> 0.125.0\n" +
		"› 1. Update now (runs `bun install -g @openai/codex`)\n" +
		"  2. Skip\n" +
		"  3. Skip until next version\n" +
		"Press enter to continue"
}

func codexHookReviewDialogFixture() string {
	return "Hooks need review\n" +
		"  4 hooks are new or changed.\n" +
		"  Hooks can run outside the sandbox after you trust them.\n\n" +
		"› 1. Review hooks\n" +
		"  2. Trust all and continue\n" +
		"  3. Continue without trusting (hooks won't run)\n\n" +
		"  Press enter to confirm or esc to go back"
}

func mcpTrustDialogFixture() string {
	return "New MCP server found in this project: mcptest-probe\n" +
		"MCP servers may execute code or access system resources. All tool calls require approval. Learn more in the MCP documentation.\n" +
		"❯ 1. Use this MCP server\n" +
		"  2. Use this and all future MCP servers in this project\n" +
		"  3. Continue without using this MCP server\n" +
		"Enter to confirm · Esc to cancel"
}

func externalImportsDialogFixture() string {
	return "Allow external CLAUDE.md file imports?\n" +
		"This project's CLAUDE.md imports files outside the current working directory. Never allow this for third-party repositories.\n" +
		"External imports:\n" +
		"  /data/projects/gascity/AGENTS.md\n" +
		"Important: Only use Claude Code with files you trust...\n" +
		"❯ 1. Yes, allow external imports\n" +
		"  2. No, disable external imports\n" +
		"Enter to confirm · Esc to cancel"
}

func TestExitsEarlyOnPrompt(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var sent []string
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) {
			return "user@host $", nil
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs() error = %v", err)
	}
	if len(sent) != 0 {
		t.Fatalf("sent keys = %v, want none (prompt detected)", sent)
	}
}

func TestExitsEarlyOnClaudeNBSPPrompt(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var sent []string
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) {
			return "❯\u00a0", nil
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs() error = %v", err)
	}
	if len(sent) != 0 {
		t.Fatalf("sent keys = %v, want none (NBSP prompt detected)", sent)
	}
}

func TestPollsUntilDialogAppears(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var peekCount atomic.Int32
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) {
			n := peekCount.Add(1)
			if n < 3 {
				return "starting up...", nil
			}
			return "Quick safety check\ntrust this folder", nil
		},
		func(...string) error {
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs() error = %v", err)
	}
	if got := peekCount.Load(); got < 3 {
		t.Fatalf("peekCount = %d, want >= 3 (polled until dialog appeared)", got)
	}
}

func TestRespectsContextCancellation(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollInterval = 50 * time.Millisecond
	dialogPollTimeout = 5 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := AcceptStartupDialogs(
		ctx,
		func(_ int) (string, error) {
			return "loading...", nil
		},
		func(...string) error {
			return nil
		},
	)
	if err == nil {
		t.Fatal("expected context error, got nil")
	}
}

func TestAcceptStartupDialogsDismissesRateLimitDialog(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var sent []string
	call := 0
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) {
			call++
			if call <= 2 {
				return "normal startup output", nil
			}
			return "Usage limit reached for gemini-3-flash-preview.\n1. Keep trying\n2. Stop", nil
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs() error = %v", err)
	}
	// Should select "Stop" (Down + Enter).
	if !reflect.DeepEqual(sent, []string{"Down", "Enter"}) {
		t.Fatalf("sent keys = %v, want [Down Enter]", sent)
	}
}

func TestContainsRateLimitDialog(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{name: "gemini usage limit", content: "Usage limit reached for gemini-3-flash-preview.", want: true},
		{name: "claude hit limit", content: "You've hit your limit, Pro plan", want: true},
		{name: "claude rate limit options", content: "/rate-limit-options", want: true},
		{name: "generic rate limit", content: "rate limit exceeded", want: true},
		{name: "Rate limit caps", content: "Rate limit: try again later", want: true},
		{name: "normal output", content: "Hello world", want: false},
		{name: "empty", content: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ContainsRateLimitDialog(tt.content); got != tt.want {
				t.Errorf("ContainsRateLimitDialog(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

// spendLimitTokensScatteredScrollback simulates a pane that merely happens to
// contain the spend-limit modal's three anchor tokens on unrelated, far-apart
// scrollback lines (e.g. a session paging through these test fixtures). All
// three tokens are present, but no small window of consecutive lines holds them
// together, so this must NOT be classified as a rate-limit screen — otherwise a
// crashed session viewing this content would be wrongly quarantined and its
// crash masked.
const spendLimitTokensScatteredScrollback = `$ less internal/runtime/dialog_test.go
comment: the fixture mentions Usage credit balance in a doc comment here
scrollback line unrelated to any modal
scrollback line unrelated to any modal
scrollback line unrelated to any modal
scrollback line unrelated to any modal
scrollback line unrelated to any modal
scrollback line unrelated to any modal
scrollback line unrelated to any modal
scrollback line unrelated to any modal
scrollback line unrelated to any modal
scrollback line unrelated to any modal
comment: another fixture names Adjust monthly spend limit as a menu option
scrollback line unrelated to any modal
scrollback line unrelated to any modal
scrollback line unrelated to any modal
scrollback line unrelated to any modal
scrollback line unrelated to any modal
scrollback line unrelated to any modal
scrollback line unrelated to any modal
scrollback line unrelated to any modal
scrollback line unrelated to any modal
scrollback line unrelated to any modal
comment: and a third names Wait for limit to reset as the confirm arm`

func TestContainsProviderRateLimitScreen(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{name: "gemini usage limit", content: "Usage limit reached for gemini-3-flash-preview.", want: true},
		{name: "claude hit limit", content: "You've hit your limit, Pro plan", want: true},
		{name: "claude rate limit options", content: "/rate-limit-options", want: true},
		{name: "provider menu shape", content: "Rate limit reached\n1. Keep trying\n2. Stop", want: true},
		{name: "claude spend limit modal", content: "What do you want to do?\nUsage credit balance: $573.37\n❯ Adjust monthly spend limit: $1503.19\n  Wait for limit to reset      Resets Jul 12 at 11pm (America/Los_Angeles)\nEnter to confirm · Esc to cancel", want: true},
		{name: "spend limit words without reset option", content: "notes mention Adjust monthly spend limit and Usage credit balance while documenting billing", want: false},
		{name: "spend limit tokens scattered across unrelated scrollback", content: spendLimitTokensScatteredScrollback, want: false},
		{name: "generic crash output", content: "worker failed while parsing rate limit config", want: false},
		{name: "generic lower-case mention", content: "rate limit exceeded", want: false},
		{name: "normal output", content: "Hello world", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ContainsProviderRateLimitScreen(tt.content); got != tt.want {
				t.Errorf("ContainsProviderRateLimitScreen(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

func TestProviderTerminalErrorReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "codex model not found code", content: "model_not_found: gpt-5.3-codex-spark", want: "model_not_found"},
		{name: "model not found text", content: "Error: model gpt-x was not found", want: "model_not_found"},
		{name: "model and not-found on different lines is not terminal", content: "loading model weights\n... file path not found", want: ""},
		{name: "quota exceeded", content: "Error: quota exceeded", want: "quota_exceeded"},
		{name: "insufficient quota", content: "insufficient_quota: billing required", want: "quota_exceeded"},
		{name: "disk quota is not provider quota", content: "disk quota exceeded while writing log", want: ""},
		{name: "generic rate limit remains transient", content: "Rate limit reached\n1. Keep trying\n2. Stop", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ProviderTerminalErrorReason(tt.content); got != tt.want {
				t.Errorf("ProviderTerminalErrorReason(%q) = %q, want %q", tt.content, got, tt.want)
			}
		})
	}
}

func TestContainsCustomAPIKeyDialog(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "custom api key prompt",
			content: "Detected a custom API key in your environment\nDo you want to use this API key?",
			want:    true,
		},
		{
			name:    "question only",
			content: "Do you want to use this API key?",
			want:    true,
		},
		{
			name:    "normal output",
			content: "Starting Claude Code...",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containsCustomAPIKeyDialog(tt.content); got != tt.want {
				t.Fatalf("containsCustomAPIKeyDialog(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}
