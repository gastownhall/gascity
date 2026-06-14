package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// errFakeResolver is a sentinel error returned by the fake DefaultBranch
// resolver to exercise the error-fallback path.
var errFakeResolver = errors.New("fake resolver failure")

// fakeAgentWorktreeGit is a configurable fake for the agentWorktreeGitProbe interface.
type fakeAgentWorktreeGit struct {
	isRepo            bool
	currentBranch     string
	currentBranchErr  error
	hasUncommitted    bool
	checkoutDetachErr error
	checkoutDetachRef string
	defaultBranch     string
	defaultBranchErr  error
}

func (f *fakeAgentWorktreeGit) IsRepo() bool { return f.isRepo }

func (f *fakeAgentWorktreeGit) CurrentBranch() (string, error) {
	return f.currentBranch, f.currentBranchErr
}

func (f *fakeAgentWorktreeGit) HasUncommittedWork() bool { return f.hasUncommitted }

func (f *fakeAgentWorktreeGit) CheckoutDetach(ref string) error {
	f.checkoutDetachRef = ref
	return f.checkoutDetachErr
}

func (f *fakeAgentWorktreeGit) DefaultBranch() (string, error) {
	return f.defaultBranch, f.defaultBranchErr
}

func setupAgentHomeWorktreeCleanupTest(t *testing.T) (cityPath, builderWTPath string, store beads.Store) {
	t.Helper()
	cityPath = t.TempDir()
	rigWTDir := filepath.Join(cityPath, ".gc", "worktrees", "ga-rig")
	builderWTPath = filepath.Join(rigWTDir, "builder")
	if err := os.MkdirAll(builderWTPath, 0o755); err != nil {
		t.Fatalf("creating builder worktree: %v", err)
	}
	store = beads.NewMemStore()
	return
}

func agentHomeConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test", Prefix: "ga"},
		Agents:    []config.Agent{{Name: "builder", Dir: "ga-rig"}},
	}
}

// TestBeadIDFromBranch_BareID: "ga-frmdxd" → "ga-frmdxd".
func TestBeadIDFromBranch_BareID(t *testing.T) {
	cfg := gaConfig()
	got := beadIDFromBranch(cfg, "ga-frmdxd")
	if got != "ga-frmdxd" {
		t.Errorf("got %q, want %q", got, "ga-frmdxd")
	}
}

// TestBeadIDFromBranch_WithAgentPrefix: "builder/ga-frmdxd.3" → "ga-frmdxd.3"
// (child bead IDs are returned as-is; the caller resolves them in the store).
func TestBeadIDFromBranch_WithAgentPrefix(t *testing.T) {
	cfg := gaConfig()
	got := beadIDFromBranch(cfg, "builder/ga-frmdxd.3")
	if got != "ga-frmdxd.3" {
		t.Errorf("got %q, want %q", got, "ga-frmdxd.3")
	}
}

// TestBeadIDFromBranch_WithDescriptiveSuffix: "builder/ga-abc123-some-feature" → "ga-abc123".
func TestBeadIDFromBranch_WithDescriptiveSuffix(t *testing.T) {
	cfg := gaConfig()
	got := beadIDFromBranch(cfg, "builder/ga-abc123-some-feature")
	if got != "ga-abc123" {
		t.Errorf("got %q, want %q", got, "ga-abc123")
	}
}

// TestBeadIDFromBranch_Detached: "HEAD" → "".
func TestBeadIDFromBranch_Detached(t *testing.T) {
	cfg := gaConfig()
	got := beadIDFromBranch(cfg, "HEAD")
	if got != "" {
		t.Errorf("got %q, want empty for HEAD", got)
	}
}

// TestBeadIDFromBranch_NoBeadID: no valid bead ID in branch name → "".
func TestBeadIDFromBranch_NoBeadID(t *testing.T) {
	cfg := gaConfig()
	got := beadIDFromBranch(cfg, "main")
	if got != "" {
		t.Errorf("got %q, want empty for non-bead branch", got)
	}
}

// TestBeadIDFromBranch_Empty: "" → "".
func TestBeadIDFromBranch_Empty(t *testing.T) {
	cfg := gaConfig()
	got := beadIDFromBranch(cfg, "")
	if got != "" {
		t.Errorf("got %q, want empty for empty branch", got)
	}
}

// TestCleanupClosedBeadAgentHomeWorktrees_SkipsWithoutMarker verifies that
// worktrees without a .worktree-stale marker are left untouched.
func TestCleanupClosedBeadAgentHomeWorktrees_SkipsWithoutMarker(t *testing.T) {
	cityPath, builderWTPath, store := setupAgentHomeWorktreeCleanupTest(t)
	cfg := agentHomeConfig()

	var fakeGit *fakeAgentWorktreeGit
	orig := newAgentWorktreeGitProbe
	defer func() { newAgentWorktreeGitProbe = orig }()
	newAgentWorktreeGitProbe = func(_ string) agentWorktreeGitProbe {
		fakeGit = &fakeAgentWorktreeGit{isRepo: true, currentBranch: "HEAD"}
		return fakeGit
	}

	cleaned := cleanupClosedBeadAgentHomeWorktrees(cityPath, cfg, map[string]beads.Store{"ga-rig": store}, nil)
	if cleaned != 0 {
		t.Errorf("cleaned = %d, want 0 when no marker present", cleaned)
	}
	// Confirm no marker was created.
	if _, err := os.Stat(filepath.Join(builderWTPath, worktreeStaleFileName)); !os.IsNotExist(err) {
		t.Error("marker appeared unexpectedly")
	}
}

// TestCleanupClosedBeadAgentHomeWorktrees_SkipsNonSessionHomes verifies that
// non-session-home directories (per-bead worktrees) are not touched.
func TestCleanupClosedBeadAgentHomeWorktrees_SkipsNonSessionHomes(t *testing.T) {
	cityPath := t.TempDir()
	// "ga-abc123" is a per-bead worktree, not a session home.
	perBeadWT := filepath.Join(cityPath, ".gc", "worktrees", "ga-rig", "ga-abc123")
	if err := os.MkdirAll(perBeadWT, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stalePath := filepath.Join(perBeadWT, worktreeStaleFileName)
	if err := os.WriteFile(stalePath, []byte("branch=builder/ga-abc123\n"), 0o644); err != nil {
		t.Fatalf("write stale: %v", err)
	}

	cfg := agentHomeConfig() // builder is the session home, not ga-abc123
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-abc123", Status: "closed"}}, nil)

	orig := newAgentWorktreeGitProbe
	defer func() { newAgentWorktreeGitProbe = orig }()
	newAgentWorktreeGitProbe = func(_ string) agentWorktreeGitProbe {
		return &fakeAgentWorktreeGit{isRepo: true, currentBranch: "builder/ga-abc123"}
	}

	cleaned := cleanupClosedBeadAgentHomeWorktrees(cityPath, cfg, map[string]beads.Store{"ga-rig": store}, nil)
	if cleaned != 0 {
		t.Errorf("cleaned = %d, want 0: per-bead worktrees must be skipped", cleaned)
	}
	if _, err := os.Stat(stalePath); err != nil {
		t.Error("stale marker was removed from per-bead worktree, want untouched")
	}
}

// TestCleanupClosedBeadAgentHomeWorktrees_CaseA_DetachedHeadRemovesMarker verifies
// that a .worktree-stale marker is removed when the worktree is already detached
// (currentBranch == "HEAD"), regardless of the marker's recorded ahead count.
func TestCleanupClosedBeadAgentHomeWorktrees_CaseA_DetachedHeadRemovesMarker(t *testing.T) {
	cityPath, builderWTPath, store := setupAgentHomeWorktreeCleanupTest(t)
	cfg := agentHomeConfig()
	stalePath := filepath.Join(builderWTPath, worktreeStaleFileName)
	if err := os.WriteFile(stalePath, []byte("branch=builder/ga-frmdxd.3\nbase=origin/main\nahead=0\nreason=rebase-onto-main-conflicted\n"), 0o644); err != nil {
		t.Fatalf("write stale marker: %v", err)
	}

	orig := newAgentWorktreeGitProbe
	defer func() { newAgentWorktreeGitProbe = orig }()
	newAgentWorktreeGitProbe = func(_ string) agentWorktreeGitProbe {
		return &fakeAgentWorktreeGit{isRepo: true, currentBranch: "HEAD"}
	}

	var stderr bytes.Buffer
	cleaned := cleanupClosedBeadAgentHomeWorktrees(cityPath, cfg, map[string]beads.Store{"ga-rig": store}, &stderr)

	if cleaned != 1 {
		t.Errorf("cleaned = %d, want 1 when detached HEAD and marker present", cleaned)
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Error("stale marker not removed for detached HEAD worktree")
	}
}

// TestCleanupClosedBeadAgentHomeWorktrees_CaseB_ClosedBeadResetsAndRemovesMarker
// verifies that the worktree is reset to detached origin/main and the marker
// is removed when the current branch corresponds to a confirmed-closed bead.
func TestCleanupClosedBeadAgentHomeWorktrees_CaseB_ClosedBeadResetsAndRemovesMarker(t *testing.T) {
	cityPath, builderWTPath, _ := setupAgentHomeWorktreeCleanupTest(t)
	cfg := agentHomeConfig()
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-abc123", Status: "closed"}}, nil)

	stalePath := filepath.Join(builderWTPath, worktreeStaleFileName)
	if err := os.WriteFile(stalePath, []byte("branch=builder/ga-abc123\nbase=origin/main\nahead=3\nreason=rebase-onto-main-conflicted\n"), 0o644); err != nil {
		t.Fatalf("write stale marker: %v", err)
	}

	var fake *fakeAgentWorktreeGit
	orig := newAgentWorktreeGitProbe
	defer func() { newAgentWorktreeGitProbe = orig }()
	newAgentWorktreeGitProbe = func(_ string) agentWorktreeGitProbe {
		fake = &fakeAgentWorktreeGit{
			isRepo:        true,
			currentBranch: "builder/ga-abc123",
		}
		return fake
	}

	var stderr bytes.Buffer
	cleaned := cleanupClosedBeadAgentHomeWorktrees(cityPath, cfg, map[string]beads.Store{"ga-rig": store}, &stderr)

	if cleaned != 1 {
		t.Errorf("cleaned = %d, want 1 when bead closed", cleaned)
	}
	if fake.checkoutDetachRef != "origin/main" {
		t.Errorf("CheckoutDetach(%q), want %q", fake.checkoutDetachRef, "origin/main")
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Error("stale marker not removed after reset for closed bead")
	}
}

// TestCleanupClosedBeadAgentHomeWorktrees_CaseB_OpenBeadSkips verifies that
// the worktree is left untouched when the bead is not closed.
func TestCleanupClosedBeadAgentHomeWorktrees_CaseB_OpenBeadSkips(t *testing.T) {
	cityPath, builderWTPath, _ := setupAgentHomeWorktreeCleanupTest(t)
	cfg := agentHomeConfig()
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-abc123", Status: "open"}}, nil)

	stalePath := filepath.Join(builderWTPath, worktreeStaleFileName)
	if err := os.WriteFile(stalePath, []byte("branch=builder/ga-abc123\n"), 0o644); err != nil {
		t.Fatalf("write stale marker: %v", err)
	}

	orig := newAgentWorktreeGitProbe
	defer func() { newAgentWorktreeGitProbe = orig }()
	newAgentWorktreeGitProbe = func(_ string) agentWorktreeGitProbe {
		return &fakeAgentWorktreeGit{isRepo: true, currentBranch: "builder/ga-abc123"}
	}

	cleaned := cleanupClosedBeadAgentHomeWorktrees(cityPath, cfg, map[string]beads.Store{"ga-rig": store}, nil)
	if cleaned != 0 {
		t.Errorf("cleaned = %d, want 0 for open bead", cleaned)
	}
	if _, err := os.Stat(stalePath); err != nil {
		t.Error("stale marker removed for open bead, want untouched")
	}
}

// TestCleanupClosedBeadAgentHomeWorktrees_CaseB_UncommittedWorkSkips verifies
// that a worktree with uncommitted changes is never reset even if the bead is closed.
func TestCleanupClosedBeadAgentHomeWorktrees_CaseB_UncommittedWorkSkips(t *testing.T) {
	cityPath, builderWTPath, _ := setupAgentHomeWorktreeCleanupTest(t)
	cfg := agentHomeConfig()
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-abc123", Status: "closed"}}, nil)

	stalePath := filepath.Join(builderWTPath, worktreeStaleFileName)
	if err := os.WriteFile(stalePath, []byte("branch=builder/ga-abc123\n"), 0o644); err != nil {
		t.Fatalf("write stale marker: %v", err)
	}

	var fake *fakeAgentWorktreeGit
	orig := newAgentWorktreeGitProbe
	defer func() { newAgentWorktreeGitProbe = orig }()
	newAgentWorktreeGitProbe = func(_ string) agentWorktreeGitProbe {
		fake = &fakeAgentWorktreeGit{
			isRepo:         true,
			currentBranch:  "builder/ga-abc123",
			hasUncommitted: true,
		}
		return fake
	}

	var stderr bytes.Buffer
	cleaned := cleanupClosedBeadAgentHomeWorktrees(cityPath, cfg, map[string]beads.Store{"ga-rig": store}, &stderr)

	if cleaned != 0 {
		t.Errorf("cleaned = %d, want 0 when uncommitted work present", cleaned)
	}
	if fake.checkoutDetachRef != "" {
		t.Error("CheckoutDetach was called, want skipped when uncommitted work present")
	}
	if _, err := os.Stat(stalePath); err != nil {
		t.Error("stale marker removed despite uncommitted work, want untouched")
	}
	if !strings.Contains(stderr.String(), "uncommitted") {
		t.Errorf("stderr = %q, want mention of uncommitted work", stderr.String())
	}
}

// TestCleanupClosedBeadAgentHomeWorktrees_NilConfig returns 0 gracefully.
func TestCleanupClosedBeadAgentHomeWorktrees_NilConfig(t *testing.T) {
	cleaned := cleanupClosedBeadAgentHomeWorktrees(t.TempDir(), nil, map[string]beads.Store{}, nil)
	if cleaned != 0 {
		t.Errorf("cleaned = %d, want 0 for nil config", cleaned)
	}
}

// TestCleanupClosedBeadAgentHomeWorktrees_EmptyStores returns 0 gracefully.
func TestCleanupClosedBeadAgentHomeWorktrees_EmptyStores(t *testing.T) {
	cfg := agentHomeConfig()
	cleaned := cleanupClosedBeadAgentHomeWorktrees(t.TempDir(), cfg, nil, nil)
	if cleaned != 0 {
		t.Errorf("cleaned = %d, want 0 for empty stores", cleaned)
	}
}

// TestCleanupClosedBeadAgentHomeWorktrees_DefaultBranch verifies that Case B
// uses the probed default branch for the detach reset ref.
func TestCleanupClosedBeadAgentHomeWorktrees_DefaultBranch(t *testing.T) {
	cases := []struct {
		name             string
		defaultBranch    string
		defaultBranchErr error
		wantRef          string
	}{
		{name: "non-main default branch", defaultBranch: "master", wantRef: "origin/master"},
		{name: "custom default branch", defaultBranch: "develop", wantRef: "origin/develop"},
		{name: "resolver returns empty, fallback to main", defaultBranch: "", wantRef: "origin/main"},
		{name: "resolver error, fallback to main", defaultBranchErr: errFakeResolver, wantRef: "origin/main"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, builderWTPath, _ := setupAgentHomeWorktreeCleanupTest(t)
			cfg := agentHomeConfig()
			store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-abc123", Status: "closed"}}, nil)

			stalePath := filepath.Join(builderWTPath, worktreeStaleFileName)
			if err := os.WriteFile(stalePath, []byte("branch=builder/ga-abc123\n"), 0o644); err != nil {
				t.Fatalf("write stale marker: %v", err)
			}

			var fake *fakeAgentWorktreeGit
			orig := newAgentWorktreeGitProbe
			defer func() { newAgentWorktreeGitProbe = orig }()
			newAgentWorktreeGitProbe = func(_ string) agentWorktreeGitProbe {
				fake = &fakeAgentWorktreeGit{
					isRepo:           true,
					currentBranch:    "builder/ga-abc123",
					defaultBranch:    tc.defaultBranch,
					defaultBranchErr: tc.defaultBranchErr,
				}
				return fake
			}

			cleaned := cleanupClosedBeadAgentHomeWorktrees(cityPath, cfg, map[string]beads.Store{"ga-rig": store}, nil)
			if cleaned != 1 {
				t.Errorf("cleaned = %d, want 1", cleaned)
			}
			if fake.checkoutDetachRef != tc.wantRef {
				t.Errorf("CheckoutDetach(%q), want %q", fake.checkoutDetachRef, tc.wantRef)
			}
		})
	}
}

// TestCleanupClosedBeadAgentHomeWorktrees_DetachesToMainNotCurrentBranch is a
// regression test for the origin/HEAD-unset case. The mainline resolver
// (DefaultBranch) must never return the current (closed bead) branch, so the
// reset ref must be origin/main — never origin/<current branch>. This guards
// against reintroducing the registration-time ProbeDefaultBranch resolver,
// which falls back to the current branch when origin/HEAD is unset.
func TestCleanupClosedBeadAgentHomeWorktrees_DetachesToMainNotCurrentBranch(t *testing.T) {
	cityPath, builderWTPath, _ := setupAgentHomeWorktreeCleanupTest(t)
	cfg := agentHomeConfig()
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-abc123", Status: "closed"}}, nil)

	stalePath := filepath.Join(builderWTPath, worktreeStaleFileName)
	if err := os.WriteFile(stalePath, []byte("branch=builder/ga-abc123\n"), 0o644); err != nil {
		t.Fatalf("write stale marker: %v", err)
	}

	var fake *fakeAgentWorktreeGit
	orig := newAgentWorktreeGitProbe
	defer func() { newAgentWorktreeGitProbe = orig }()
	newAgentWorktreeGitProbe = func(_ string) agentWorktreeGitProbe {
		fake = &fakeAgentWorktreeGit{
			isRepo:        true,
			currentBranch: "builder/ga-abc123",
			// DefaultBranch resolves origin/HEAD → origin/main → origin/master
			// → "main"; it never returns the current branch. Simulate the
			// origin/HEAD-unset case where it resolves to "main".
			defaultBranch: "main",
		}
		return fake
	}

	cleaned := cleanupClosedBeadAgentHomeWorktrees(cityPath, cfg, map[string]beads.Store{"ga-rig": store}, nil)
	if cleaned != 1 {
		t.Errorf("cleaned = %d, want 1", cleaned)
	}
	if fake.checkoutDetachRef != "origin/main" {
		t.Errorf("CheckoutDetach(%q), want %q", fake.checkoutDetachRef, "origin/main")
	}
	if fake.checkoutDetachRef == "origin/builder/ga-abc123" {
		t.Error("reset detached to the closed bead branch; must reset to origin/main")
	}
}
