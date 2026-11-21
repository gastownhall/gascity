package beads

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// The creator is resolved once, when the store is built, and it reads the
// ambient environment through AmbientNativeDoltOpenEnv. A native open elsewhere
// in the process holds nativeDoltOpenEnvMu and withholds the whole BEADS_ and BD_
// namespaces for its window; a bare os.Getenv would see BEADS_ACTOR unset there
// and fall through to git or USER. The store must instead wait the window out
// and take the restored ambient value.
func TestNativeDoltStoreCreatorResolvesAmbientActorAfterOpenWindow(t *testing.T) {
	const ambient = "order:window-test"
	t.Setenv("BEADS_ACTOR", ambient)
	t.Setenv("BD_ACTOR", "")

	end := holdOpenWindow(t)

	built := make(chan *NativeDoltStore, 1)
	go func() { built <- newNativeDoltStoreWithStorage(nil, "") }()

	select {
	case store := <-built:
		end()
		t.Fatalf("store built inside the open window with creator %q; it must wait on nativeDoltOpenEnvMu", store.creator)
	case <-time.After(50 * time.Millisecond):
	}
	end()

	select {
	case store := <-built:
		if store.creator != ambient {
			t.Errorf("creator = %q, want the restored ambient BEADS_ACTOR %q", store.creator, ambient)
		}
	case <-time.After(openWindowPatience):
		t.Fatal("store was not built after the open window ended")
	}
}

// WithNativeCreator overrides whatever the ambient environment says.
func TestNativeDoltStoreWithNativeCreatorOverridesAmbient(t *testing.T) {
	t.Setenv("BEADS_ACTOR", "order:ambient")
	store := newNativeDoltStoreWithStorage(nil, "")
	WithNativeCreator("pinned-creator")(store)
	if store.creator != "pinned-creator" {
		t.Errorf("creator = %q, want %q", store.creator, "pinned-creator")
	}
}

// The tests below were added by the reviewer (gm-4xiq97). They pin the rest of
// the creator contract without a Dolt server: the resolution chain, the
// guarded-batch and over-cap create doors, the sender mirror on write and on
// read, and the rule that the ownership-fence actor is never the creator.

const (
	reviewerTestCreator = "creator-under-test"
	reviewerTestSender  = "human"
	reviewerTestActor   = "native-test"
)

// isolateNativeCreatorSources empties every source nativeCreatorIdentity reads
// and points PATH at an empty directory, so git is not found and cannot answer
// from the host's own configuration. A case then sets only the sources it means.
func isolateNativeCreatorSources(t *testing.T) {
	t.Helper()
	t.Setenv("BEADS_ACTOR", "")
	t.Setenv("BD_ACTOR", "")
	t.Setenv("USER", "")
	t.Setenv("PATH", t.TempDir())
}

// The chain mirrors bd's getActorWithGit: BEADS_ACTOR, BD_ACTOR, git config
// user.name, USER. A blank value does not count and the winner is trimmed.
func TestNativeCreatorIdentityResolutionOrder(t *testing.T) {
	cases := []struct {
		name       string
		beadsActor string
		bdActor    string
		user       string
		want       string
	}{
		{name: "BEADS_ACTOR wins over BD_ACTOR and USER", beadsActor: "beads-actor", bdActor: "bd-actor", user: "os-user", want: "beads-actor"},
		{name: "BD_ACTOR when BEADS_ACTOR is empty", bdActor: "bd-actor", user: "os-user", want: "bd-actor"},
		{name: "BD_ACTOR when BEADS_ACTOR is only whitespace", beadsActor: " \t", bdActor: "bd-actor", want: "bd-actor"},
		{name: "the winner is trimmed", beadsActor: "  beads-actor\n", want: "beads-actor"},
		{name: "USER when nothing else resolves", user: "os-user", want: "os-user"},
		{name: "empty when nothing resolves", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateNativeCreatorSources(t)
			t.Setenv("BEADS_ACTOR", tc.beadsActor)
			t.Setenv("BD_ACTOR", tc.bdActor)
			t.Setenv("USER", tc.user)
			if got := nativeCreatorIdentity(); got != tc.want {
				t.Errorf("nativeCreatorIdentity() = %q, want %q", got, tc.want)
			}
		})
	}
}

// git config user.name answers after BEADS_ACTOR and BD_ACTOR and before USER; a
// git that fails, or prints only whitespace, falls through to USER. The git here
// is a script alone on a PATH of its own, so no case depends on the host's git
// or its configuration.
func TestNativeCreatorIdentityGitUserNameComesBeforeUser(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake git is a POSIX shell script")
	}
	cases := []struct {
		name    string
		bdActor string
		gitBody string
		want    string
	}{
		{
			name:    "git answers when both actor variables are empty",
			gitBody: "[ \"$1 $2\" = \"config user.name\" ] || exit 2\nprintf '  git-user  \\n'\n",
			want:    "git-user",
		},
		{
			name:    "BD_ACTOR still wins over git",
			bdActor: "bd-actor",
			gitBody: "printf 'git-user\\n'\n",
			want:    "bd-actor",
		},
		{
			name:    "a failing git falls through to USER",
			gitBody: "exit 1\n",
			want:    "os-user",
		},
		{
			name:    "a git that prints only whitespace falls through to USER",
			gitBody: "printf ' \\n'\n",
			want:    "os-user",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateNativeCreatorSources(t)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"+tc.gitBody), 0o755); err != nil { //nolint:gosec // test fixture, deliberately executable
				t.Fatalf("write fake git: %v", err)
			}
			t.Setenv("PATH", dir)
			t.Setenv("BD_ACTOR", tc.bdActor)
			t.Setenv("USER", "os-user")
			if got := nativeCreatorIdentity(); got != tc.want {
				t.Errorf("nativeCreatorIdentity() = %q, want %q", got, tc.want)
			}
		})
	}
}

// stampCreator fills a missing created_by and never overwrites one the caller set.
func TestNativeStampCreatorFillsOnlyAnEmptyCreatedBy(t *testing.T) {
	store := &NativeDoltStore{creator: "store-creator"}
	empty := &beadslib.Issue{}
	store.stampCreator(empty)
	if empty.CreatedBy != "store-creator" {
		t.Errorf("an empty CreatedBy was stamped %q, want %q", empty.CreatedBy, "store-creator")
	}
	preset := &beadslib.Issue{CreatedBy: "caller"}
	store.stampCreator(preset)
	if preset.CreatedBy != "caller" {
		t.Errorf("a caller-set CreatedBy became %q, want it kept as %q", preset.CreatedBy, "caller")
	}
}

// metadataWithFrom mirrors a sender under metadata["from"], the shape BdStore
// writes. A non-empty caller-set key wins, an empty sender is a no-op, and the input map is
// never mutated.
func TestMetadataWithFrom(t *testing.T) {
	t.Run("an empty sender returns the input untouched", func(t *testing.T) {
		if got := metadataWithFrom(nil, ""); got != nil {
			t.Errorf("metadataWithFrom(nil, \"\") = %v, want nil", got)
		}
		in := map[string]string{"k": "v"}
		got := metadataWithFrom(in, "")
		if len(got) != 1 || got["k"] != "v" {
			t.Errorf("metadataWithFrom(%v, \"\") = %v, want it unchanged", in, got)
		}
	})
	t.Run("a sender is mirrored into a copy, never into the input", func(t *testing.T) {
		in := map[string]string{"k": "v"}
		got := metadataWithFrom(in, reviewerTestSender)
		if got["from"] != reviewerTestSender || got["k"] != "v" {
			t.Errorf("metadataWithFrom = %v, want k=v plus from=%s", got, reviewerTestSender)
		}
		if _, mutated := in["from"]; mutated || len(in) != 1 {
			t.Errorf("the input map was mutated: %v", in)
		}
	})
	t.Run("a sender over nil metadata builds the map", func(t *testing.T) {
		got := metadataWithFrom(nil, reviewerTestSender)
		if len(got) != 1 || got["from"] != reviewerTestSender {
			t.Errorf("metadataWithFrom(nil, %q) = %v, want from=%s", reviewerTestSender, got, reviewerTestSender)
		}
	})
	t.Run("a caller-set from wins", func(t *testing.T) {
		in := map[string]string{"from": "caller-set"}
		got := metadataWithFrom(in, reviewerTestSender)
		if got["from"] != "caller-set" {
			t.Errorf("from = %q, want the caller-set value kept", got["from"])
		}
	})
	t.Run("an empty caller-set from is filled, as BdStore does", func(t *testing.T) {
		in := map[string]string{"from": ""}
		got := metadataWithFrom(in, reviewerTestSender)
		if got["from"] != reviewerTestSender {
			t.Errorf("from = %q, want %q filled over the empty caller-set value", got["from"], reviewerTestSender)
		}
		if v, ok := in["from"]; !ok || v != "" || len(in) != 1 {
			t.Errorf("the input map was mutated: %v", in)
		}
	})
}

// The read side takes the sender column and falls back to the "from" metadata
// key BdStore writes it under.
func TestBeadFromNativeIssueSenderFallsBackToMetadataFrom(t *testing.T) {
	cases := []struct {
		name     string
		sender   string
		metadata string
		want     string
	}{
		{name: "the column wins", sender: "column-sender", metadata: `{"from":"meta-sender"}`, want: "column-sender"},
		{name: "metadata when the column is empty", metadata: `{"from":"meta-sender"}`, want: "meta-sender"},
		{name: "neither", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issue := &beadslib.Issue{ID: "gc-sender", Sender: tc.sender}
			if tc.metadata != "" {
				issue.Metadata = json.RawMessage(tc.metadata)
			}
			got, err := beadFromNativeIssue(issue)
			if err != nil {
				t.Fatalf("beadFromNativeIssue: %v", err)
			}
			if got.From != tc.want {
				t.Errorf("From = %q, want %q", got.From, tc.want)
			}
		})
	}
}

// creatorTestMetadataFrom returns the "from" key of an issue's raw metadata, or
// "" when there is none.
func creatorTestMetadataFrom(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	if len(raw) == 0 {
		return ""
	}
	var metadata map[string]string
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatalf("unmarshaling metadata %s: %v", raw, err)
	}
	return metadata["from"]
}

// Every create ApplyGraphPlan sends carries the store's creator and the node's
// sender, in the column and mirrored under metadata["from"] (a caller-set key
// wins), while the request's actor stays the ownership-fence subject.
func TestNativeApplyGraphPlanStampsCreatorAndSenderOnEveryCreate(t *testing.T) {
	spy := newGraphApplyBatchSpy()
	store := newNativeDoltStoreForTest(spy, WithNativeCreator(reviewerTestCreator))
	plan := &GraphApplyPlan{Nodes: []GraphApplyNode{
		{Key: "mail", Title: "Mail", From: reviewerTestSender},
		{Key: "plain", Title: "Plain"},
		{Key: "kept", Title: "Kept", From: reviewerTestSender, Metadata: map[string]string{"from": "caller-set"}},
	}}
	if _, err := store.ApplyGraphPlanWithStorage(context.Background(), plan, StorageDefault); err != nil {
		t.Fatalf("ApplyGraphPlanWithStorage: %v", err)
	}
	if len(spy.recorder.requests) != 1 {
		t.Fatalf("ApplyBatch was called %d times, want 1", len(spy.recorder.requests))
	}
	req := spy.recorder.requests[0]
	if req.Actor != reviewerTestActor {
		t.Errorf("request actor = %q, want the store actor %q (the ownership-fence subject, never the creator)", req.Actor, reviewerTestActor)
	}
	wantSender := map[string]string{"mail": reviewerTestSender, "plain": "", "kept": reviewerTestSender}
	wantFrom := map[string]string{"mail": reviewerTestSender, "plain": "", "kept": "caller-set"}
	seen := 0
	for _, item := range req.Items {
		if item.Kind != issueops.ItemCreate {
			continue
		}
		seen++
		key, issue := item.Create.Key, item.Create.Issue
		if issue.CreatedBy != reviewerTestCreator {
			t.Errorf("%s: created_by = %q, want %q", key, issue.CreatedBy, reviewerTestCreator)
		}
		if issue.Sender != wantSender[key] {
			t.Errorf("%s: sender = %q, want %q", key, issue.Sender, wantSender[key])
		}
		if got := creatorTestMetadataFrom(t, issue.Metadata); got != wantFrom[key] {
			t.Errorf("%s: metadata[\"from\"] = %q, want %q", key, got, wantFrom[key])
		}
	}
	if seen != len(plan.Nodes) {
		t.Errorf("saw %d create items, want %d", seen, len(plan.Nodes))
	}
}

// A plan over the batch cap takes the local-transaction fallback, which builds
// its issues by hand. It stamps the creator and the sender the way the batch
// route does and hands the storage the store's actor, never the creator.
func TestNativeApplyGraphPlanOverCapStampsCreatorAndSender(t *testing.T) {
	spy := newGraphApplyBatchSpy()
	var created []*beadslib.Issue
	var actors []string
	spy.createIssue = func(_ context.Context, issue *beadslib.Issue, actor string) error {
		issue.ID = fmt.Sprintf("gc-local-%d", len(created)+1)
		seen := *issue
		created = append(created, &seen)
		actors = append(actors, actor)
		return nil
	}
	store := newNativeDoltStoreForTest(spy, WithNativeCreator(reviewerTestCreator))

	nodes := make([]GraphApplyNode, issueops.MaxApplyBatchItems+1)
	for i := range nodes {
		nodes[i] = GraphApplyNode{Key: fmt.Sprintf("n%d", i), Title: fmt.Sprintf("Node %d", i)}
	}
	nodes[0].From = reviewerTestSender
	if _, err := store.ApplyGraphPlanWithStorage(context.Background(), &GraphApplyPlan{Nodes: nodes}, StorageDefault); err != nil {
		t.Fatalf("ApplyGraphPlanWithStorage: %v", err)
	}
	if len(spy.recorder.requests) != 0 {
		t.Fatalf("ApplyBatch was called %d times; an over-cap plan must take the local fallback", len(spy.recorder.requests))
	}
	if len(created) != len(nodes) {
		t.Fatalf("the local fallback created %d issues, want %d", len(created), len(nodes))
	}
	for i, issue := range created {
		bad := false
		if issue.CreatedBy != reviewerTestCreator {
			t.Errorf("issue %d (%s): created_by = %q, want %q", i, issue.Title, issue.CreatedBy, reviewerTestCreator)
			bad = true
		}
		if actors[i] != reviewerTestActor {
			t.Errorf("issue %d (%s): the storage was handed actor %q, want the store actor %q", i, issue.Title, actors[i], reviewerTestActor)
			bad = true
		}
		wantSender := ""
		if issue.Title == "Node 0" {
			wantSender = reviewerTestSender
		}
		if issue.Sender != wantSender {
			t.Errorf("issue %d (%s): sender = %q, want %q", i, issue.Title, issue.Sender, wantSender)
			bad = true
		}
		if got := creatorTestMetadataFrom(t, issue.Metadata); got != wantSender {
			t.Errorf("issue %d (%s): metadata[\"from\"] = %q, want %q", i, issue.Title, got, wantSender)
			bad = true
		}
		if bad {
			// One issue shows the defect; the rest of the plan would only repeat it.
			break
		}
	}
}

// A guarded-batch create gets the same attribution as every other create door:
// the store's creator, and the sender in the column and under metadata["from"].
func TestNativeGuardedItemsStampCreatorAndSenderOnCreates(t *testing.T) {
	store := &NativeDoltStore{creator: reviewerTestCreator}
	// Create-only items never read, so the batch needs no storage.
	items, err := store.nativeGuardedItems(context.Background(), nil, GuardedBatch{Items: []GuardedItem{
		{Create: &Bead{ID: "gc-guarded-1", Title: "guarded mail", From: reviewerTestSender}},
		{Create: &Bead{ID: "gc-guarded-2", Title: "guarded plain"}},
	}})
	if err != nil {
		t.Fatalf("nativeGuardedItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("nativeGuardedItems returned %d items, want 2", len(items))
	}
	wantSender := []string{reviewerTestSender, ""}
	for i, item := range items {
		issue := item.Create.Issue
		if issue.CreatedBy != reviewerTestCreator {
			t.Errorf("item %d: created_by = %q, want %q", i, issue.CreatedBy, reviewerTestCreator)
		}
		if issue.Sender != wantSender[i] {
			t.Errorf("item %d: sender = %q, want %q", i, issue.Sender, wantSender[i])
		}
		if got := creatorTestMetadataFrom(t, issue.Metadata); got != wantSender[i] {
			t.Errorf("item %d: metadata[\"from\"] = %q, want %q", i, got, wantSender[i])
		}
	}
}
