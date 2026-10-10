package scripts_test

import (
	"slices"
	"strings"
	"testing"
)

// CodeQL's Analyze (go) leg warms caches before its autobuild (S7 in the
// gascity CI speed plan): setup-go and the shared go-mod-download composite
// before Initialize CodeQL, and a Go build cache that only a push to main
// writes when Blacksmith's GOCACHEPROG does not already serve one. The Go
// autobuilder extracts ./... through go/packages from source, so a warm
// cache never drops a package from the database. The other legs run none
// of these steps.
func TestCodeQLGoLegCaches(t *testing.T) {
	wf := readMergeQueueWorkflow(t, mergeQueueCodeQLYML)
	job, ok := wf.Jobs["analyze"]
	if !ok {
		t.Fatalf("%s has no analyze job", mergeQueueCodeQLYML)
	}
	index := map[string]int{}
	byKey := map[string]mqStep{}
	for i, s := range job.Steps {
		index[mqStepKey(s)] = i
		byKey[mqStepKey(s)] = s
	}
	order := []string{"Checkout", "Set up Go", "Restore Go modules", "gocache", "gocache-restore", "Initialize CodeQL", "Autobuild", "Save Go build cache", "Perform CodeQL Analysis"}
	for i, k := range order {
		if _, ok := index[k]; !ok {
			t.Fatalf("analyze has no step %q", k)
		}
		if i > 0 && index[k] < index[order[i-1]] {
			t.Errorf("analyze step %q runs before %q; want order %v", k, order[i-1], order)
		}
	}

	const goLeg = "matrix.language == 'go'"
	for _, k := range []string{"Set up Go", "Restore Go modules", "gocache", "gocache-restore", "Save Go build cache"} {
		if !strings.HasPrefix(byKey[k].If, goLeg) {
			t.Errorf("analyze step %q if %q; want it to start with %s (the other legs build nothing)", k, byKey[k].If, goLeg)
		}
	}
	if s := byKey["Set up Go"]; !strings.HasPrefix(s.Uses, "actions/setup-go@") || s.With["go-version-file"] != "go.mod" || s.With["cache"] != "false" {
		t.Errorf("Set up Go: uses %q with %v; want actions/setup-go, go-version-file go.mod, cache false (go-mod-download owns the module cache)", s.Uses, s.With)
	}
	if got := byKey["Restore Go modules"].Uses; got != "./.github/actions/go-mod-download" {
		t.Errorf("Restore Go modules uses %q, want the shared ./.github/actions/go-mod-download", got)
	}
	if run := byKey["gocache"].Run; !strings.Contains(run, `go env GOCACHEPROG`) || !strings.Contains(run, `go env GOCACHE)`) {
		t.Errorf("gocache step must detect GOCACHEPROG and resolve GOCACHE with go env:\n%s", run)
	}

	const key = "codeql-go-build-v1-${{ runner.os }}-${{ hashFiles('go.mod', 'go.sum') }}"
	restore, save := byKey["gocache-restore"], byKey["Save Go build cache"]
	if !strings.HasPrefix(restore.Uses, "actions/cache/restore@") || restore.With["key"] != key || restore.With["path"] != "${{ steps.gocache.outputs.dir }}" {
		t.Errorf("gocache-restore: uses %q with %v; want actions/cache/restore of steps.gocache.outputs.dir under %s", restore.Uses, restore.With, key)
	}
	if want := goLeg + " && steps.gocache.outputs.managed == 'false'"; restore.If != want {
		t.Errorf("gocache-restore if %q, want %q (GOCACHEPROG already serves Blacksmith runs)", restore.If, want)
	}
	if !strings.HasPrefix(save.Uses, "actions/cache/save@") || save.With["key"] != key || save.With["path"] != restore.With["path"] {
		t.Errorf("Save Go build cache: uses %q with %v; want actions/cache/save of the restored path under %s", save.Uses, save.With, key)
	}
	// Writers: a push to main that missed the exact key, never a PR or a
	// merge group (their code is not yet the default branch's).
	for _, c := range []struct {
		ctx  map[string]string
		save bool
	}{
		{map[string]string{"github.event_name": "push", "github.ref": "refs/heads/main"}, true},
		{map[string]string{"github.event_name": "push", "github.ref": "refs/heads/main", "steps.gocache-restore.outputs.cache-hit": "true"}, false},
		{map[string]string{"github.event_name": "push", "github.ref": "refs/heads/main", "steps.gocache.outputs.managed": "true"}, false},
		{map[string]string{"github.event_name": "pull_request", "github.ref": "refs/pull/1/merge"}, false},
		{map[string]string{"github.event_name": "merge_group", "github.ref": mergeQueueRef}, false},
		{map[string]string{"github.event_name": "schedule", "github.ref": "refs/heads/main"}, false},
		{map[string]string{"github.event_name": "push", "github.ref": "refs/heads/main", "matrix.language": "python"}, false},
	} {
		ctx := map[string]string{"matrix.language": "go", "steps.gocache.outputs.managed": "false"}
		for k, v := range c.ctx {
			ctx[k] = v
		}
		if got := evalGHIf(t, save.If, ctx); got != c.save {
			t.Errorf("Save Go build cache with %v: runs %v, want %v", c.ctx, got, c.save)
		}
	}
	// Only the go leg autobuilds.
	if got := byKey["Autobuild"].If; got != "matrix.build-mode == 'autobuild'" {
		t.Errorf("Autobuild if %q", got)
	}
	var include []map[string]string
	if err := job.Strategy.Matrix.Include.Decode(&include); err != nil {
		t.Fatal(err)
	}
	var autobuild []string
	for _, e := range include {
		if e["build-mode"] == "autobuild" {
			autobuild = append(autobuild, e["language"])
		}
	}
	if !slices.Equal(autobuild, []string{"go"}) {
		t.Errorf("autobuild legs %v, want [go]", autobuild)
	}
}
