package dispatch

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The required-artifact gate resolves {attempt} from the subject's own
// gc.attempt, which inside a ralph body is the iteration (the v1.4.2 contract),
// and the retry counter from gc.retry_attempt via {retry_attempt}.
//
// Worse, the gate is live rather than inert. A census of the maintainer-city
// graph store found 471 of 542 beads carrying a required-artifact template do
// resolve a worktree (434 through the workflow root's work_dir). So a gate
// pointed at a directory nobody wrote turns passing attempts into burned
// retries.
//
// {iteration} gives the template a key that means what the reviewers' shared
// output directory actually is: the loop iteration all of them ran in, not the
// per-step retry counter that only one of them advanced (ga-la0py).
func TestResolveRequiredArtifactPathResolvesIterationSeparatelyFromAttempt(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	worktree := t.TempDir()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"work_dir": worktree,
		},
	})

	// quality-scorecard is on retry attempt 3 of itself, inside review-loop
	// iteration 2. Its sibling reviewers never retried, so they wrote
	// attempt-2/ and the scorecard must read from there. gc.attempt carries the
	// iteration (the v1.4.2 contract); the retry counter is gc.retry_attempt.
	subject := beads.Bead{
		Metadata: map[string]string{
			"gc.outcome":       "pass",
			"gc.root_bead_id":  root.ID,
			"gc.attempt":       "2",
			"gc.retry_attempt": "3",
			"gc.iteration":     "2",
		},
	}
	resolve := func(t *testing.T, subject beads.Bead, template string) string {
		t.Helper()
		got, _, reason, err := resolveRequiredArtifactPath(store, subject, template, ProcessOptions{})
		if err != nil || reason != "" {
			t.Fatalf("resolveRequiredArtifactPath(%q) = (%v, %q), want success", template, err, reason)
		}
		return got
	}
	dir := func(segment string) string {
		return filepath.Join(worktree, ".gc/reviews", root.ID, segment, "synthesis.md")
	}

	if got := resolve(t, subject, ".gc/reviews/{root}/attempt-{iteration}/synthesis.md"); got != dir("attempt-2") {
		t.Errorf("{iteration} resolved %q, want %q", got, dir("attempt-2"))
	}

	t.Run("{attempt} keeps its v1.4.2 meaning inside a loop: the iteration", func(t *testing.T) {
		// Packs written for v1.4.2 name the iteration's shared directory with
		// {attempt}; a retried step must still land in it.
		if got := resolve(t, subject, ".gc/reviews/{root}/attempt-{attempt}/synthesis.md"); got != dir("attempt-2") {
			t.Errorf("resolved %q, want %q", got, dir("attempt-2"))
		}
	})

	t.Run("{retry_attempt} resolves the step's own retry counter", func(t *testing.T) {
		if got := resolve(t, subject, ".gc/reviews/{root}/retry-{retry_attempt}/synthesis.md"); got != dir("retry-3") {
			t.Errorf("resolved %q, want %q", got, dir("retry-3"))
		}
	})

	t.Run("{retry_attempt} falls back to gc.attempt on a bead minted before the key", func(t *testing.T) {
		legacy := beads.Bead{Metadata: map[string]string{
			"gc.outcome":      "pass",
			"gc.root_bead_id": root.ID,
			"gc.attempt":      "2",
		}}
		if got := resolve(t, legacy, ".gc/reviews/{root}/retry-{retry_attempt}/synthesis.md"); got != dir("retry-2") {
			t.Errorf("resolved %q, want %q", got, dir("retry-2"))
		}
	})
}

// A missing gc.iteration must fail loudly. Substituting empty would yield
// "attempt-/", a path nobody writes, and the gate would report
// missing_required_artifact — blaming the step for the resolver's own gap. The
// existing unresolved-template check is the correct loud failure, so the
// substitution has to be skipped rather than applied as empty.
func TestResolveRequiredArtifactPathRefusesToGuessAMissingIteration(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	worktree := t.TempDir()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"work_dir": worktree,
		},
	})

	_, _, reason, err := resolveRequiredArtifactPath(store, beads.Bead{
		Metadata: map[string]string{
			"gc.outcome":      "pass",
			"gc.root_bead_id": root.ID,
			"gc.attempt":      "3",
		},
	}, ".gc/reviews/{root}/attempt-{iteration}/synthesis.md", ProcessOptions{})
	if err != nil {
		t.Fatalf("resolveRequiredArtifactPath error = %v, want nil", err)
	}
	if reason != "unresolved_required_artifact_template" {
		t.Errorf("reason = %q, want unresolved_required_artifact_template — an absent iteration must not silently become an empty path segment", reason)
	}
}

// The worktree lookup reads the bare legacy "work_dir" and never the canonical
// gc.work_dir. beadmeta documents the read contract for this family as
// canonical-then-legacy, and internal/beads/contract implements exactly that
// for its siblings; this site predates it. Roots that carry only the canonical
// key resolve to nothing and their attempts go transient forever.
func TestResolveRequiredArtifactWorktreePrefersCanonicalWorkDir(t *testing.T) {
	t.Parallel()

	t.Run("subject's own canonical key", func(t *testing.T) {
		store := beads.NewMemStore()
		worktree := t.TempDir()
		root := mustCreateWorkflowBead(t, store, beads.Bead{
			Title:    "workflow",
			Type:     "task",
			Metadata: map[string]string{"work_dir": t.TempDir()},
		})
		got, _, reason, err := resolveRequiredArtifactPath(store, beads.Bead{
			Metadata: map[string]string{
				"gc.root_bead_id": root.ID,
				"gc.work_dir":     worktree,
			},
		}, "synthesis.md", ProcessOptions{})
		if err != nil || reason != "" {
			t.Fatalf("resolveRequiredArtifactPath = (%v, %q), want success", err, reason)
		}
		if want := filepath.Join(worktree, "synthesis.md"); got != want {
			t.Errorf("resolved %q, want %q — the subject's canonical gc.work_dir must win", got, want)
		}
	})

	t.Run("root's canonical key", func(t *testing.T) {
		store := beads.NewMemStore()
		worktree := t.TempDir()
		root := mustCreateWorkflowBead(t, store, beads.Bead{
			Title:    "workflow",
			Type:     "task",
			Metadata: map[string]string{"gc.work_dir": worktree},
		})
		got, reason, err := resolveRequiredArtifactWorktree(store, root.ID, ProcessOptions{})
		if err != nil || reason != "" {
			t.Fatalf("resolveRequiredArtifactWorktree = (%v, %q), want success", err, reason)
		}
		if got != worktree {
			t.Errorf("resolved %q, want %q", got, worktree)
		}
	})

	t.Run("source bead's canonical key", func(t *testing.T) {
		store := beads.NewMemStore()
		worktree := t.TempDir()
		source := mustCreateWorkflowBead(t, store, beads.Bead{
			Title:    "source",
			Type:     "convoy",
			Metadata: map[string]string{"gc.work_dir": worktree},
		})
		root := mustCreateWorkflowBead(t, store, beads.Bead{
			Title:    "workflow",
			Type:     "task",
			Metadata: map[string]string{"gc.source_bead_id": source.ID},
		})
		got, reason, err := resolveRequiredArtifactWorktree(store, root.ID, ProcessOptions{})
		if err != nil || reason != "" {
			t.Fatalf("resolveRequiredArtifactWorktree = (%v, %q), want success", err, reason)
		}
		if got != worktree {
			t.Errorf("resolved %q, want %q", got, worktree)
		}
	})

	t.Run("legacy key still wins when canonical is absent", func(t *testing.T) {
		// The overwhelming majority of live beads carry only the legacy key;
		// adding the canonical read must not disturb them.
		store := beads.NewMemStore()
		worktree := t.TempDir()
		root := mustCreateWorkflowBead(t, store, beads.Bead{
			Title:    "workflow",
			Type:     "task",
			Metadata: map[string]string{"work_dir": worktree},
		})
		got, reason, err := resolveRequiredArtifactWorktree(store, root.ID, ProcessOptions{})
		if err != nil || reason != "" {
			t.Fatalf("resolveRequiredArtifactWorktree = (%v, %q), want success", err, reason)
		}
		if got != worktree {
			t.Errorf("resolved %q, want %q", got, worktree)
		}
	})
}
