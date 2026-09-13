package dispatch

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The required-artifact gate resolves {attempt} from the subject's own
// gc.attempt — the same key ga-v7pu5 showed is carrying the ralph iteration
// index rather than the step's retry counter. That makes the gate structurally
// incapable of catching the artifact-directory mismatch: it computes the same
// wrong directory the failing step computed, agrees with it, and passes.
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

	// The shape that breaks today: quality-scorecard is on retry attempt 3 of
	// itself, inside review-loop iteration 2. Its sibling reviewers never
	// retried, so they wrote attempt-2/ and the scorecard must read from there.
	subject := beads.Bead{
		Metadata: map[string]string{
			"gc.outcome":      "pass",
			"gc.root_bead_id": root.ID,
			"gc.attempt":      "3",
			"gc.iteration":    "2",
		},
	}

	got, _, reason, err := resolveRequiredArtifactPath(store, subject,
		".gc/reviews/{root}/attempt-{iteration}/synthesis.md")
	if err != nil {
		t.Fatalf("resolveRequiredArtifactPath error = %v, want nil", err)
	}
	if reason != "" {
		t.Fatalf("resolveRequiredArtifactPath reason = %q, want none", reason)
	}
	want := filepath.Join(worktree, ".gc/reviews", root.ID, "attempt-2", "synthesis.md")
	if got != want {
		t.Errorf("resolved %q, want %q — {iteration} must read gc.iteration, not gc.attempt", got, want)
	}

	t.Run("{attempt} still resolves the step's own retry counter", func(t *testing.T) {
		// The two placeholders must genuinely diverge; a template that asks for
		// the retry attempt still gets it.
		got, _, reason, err := resolveRequiredArtifactPath(store, subject,
			".gc/reviews/{root}/attempt-{attempt}/synthesis.md")
		if err != nil || reason != "" {
			t.Fatalf("resolveRequiredArtifactPath = (%v, %q), want success", err, reason)
		}
		want := filepath.Join(worktree, ".gc/reviews", root.ID, "attempt-3", "synthesis.md")
		if got != want {
			t.Errorf("resolved %q, want %q", got, want)
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
	}, ".gc/reviews/{root}/attempt-{iteration}/synthesis.md")
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
		}, "synthesis.md")
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
		got, reason, err := resolveRequiredArtifactWorktree(store, root.ID)
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
		got, reason, err := resolveRequiredArtifactWorktree(store, root.ID)
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
		got, reason, err := resolveRequiredArtifactWorktree(store, root.ID)
		if err != nil || reason != "" {
			t.Fatalf("resolveRequiredArtifactWorktree = (%v, %q), want success", err, reason)
		}
		if got != worktree {
			t.Errorf("resolved %q, want %q", got, worktree)
		}
	})
}
