package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/workrecord"
)

// initGateRepo makes dir a git repository holding exactly one commit on main.
// The gate's reachability clause is a real git question, so a row claiming a
// commit is reachable on one checkout and not on another has to own two real
// repositories; a stubbed oracle would only prove the stub.
func initGateRepo(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating the checkout at %s: %v", dir, err)
	}
	runResolverGit(t, dir, "init", "--initial-branch=main")
	runResolverGit(t, dir, "config", "user.name", "Gas City Test")
	runResolverGit(t, dir, "config", "user.email", "gc-test@test.local")
	if err := os.WriteFile(filepath.Join(dir, "artifact.txt"), []byte(content+"\n"), 0o644); err != nil {
		t.Fatalf("writing the artifact in %s: %v", dir, err)
	}
	runResolverGit(t, dir, "add", "artifact.txt")
	runResolverGit(t, dir, "commit", "-m", "test: "+content)
}

// gateRepoHead reads the commit main points at out of the ref file rather than
// through another git process. This package's test-resource census pins its
// subprocess count, and a repository just initialized with one commit always
// writes main as a loose ref, so the file is the cheaper truth.
func gateRepoHead(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, ".git", "refs", "heads", "main"))
	if err != nil {
		t.Fatalf("reading main in %s: %v", dir, err)
	}
	commit := strings.TrimSpace(string(raw))
	if len(commit) != 40 {
		t.Fatalf("main in %s resolved to %q, which is not a commit SHA", dir, commit)
	}
	return commit
}

// TestAPIBeadCloseAsksTheOwningRigsCheckoutForABindingResidentBead is the
// residency half of the gate. The store a row is served FROM says where it
// lives; gc.root_store_ref says who OWNS it. A rig-owned work step that a
// relocated class binding holds has its commits on the RIG's checkout, so
// asking the city repository — which the store-identity resolution did, because
// no configured rig claims the binding — reports a landed commit as unreachable
// and refuses the close under enforcement.
//
// The city checkout here is a REAL repository that simply does not hold the
// commit. That is the control separating "asked the wrong repository" from
// "asked no repository at all": both answer not-reachable, and only the first
// is this bug.
func TestAPIBeadCloseAsksTheOwningRigsCheckoutForABindingResidentBead(t *testing.T) {
	st, binding, _, _ := relocatedGraphRouteState(t)
	rigRepo := st.cfg.Rigs[0].Path
	initGateRepo(t, rigRepo, "rig work")
	initGateRepo(t, st.cityPath, "city work")
	commit := gateRepoHead(t, rigRepo)

	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatalf("ReservedClassPrefix(graph) returned ok=false; expected a reserved prefix")
	}
	owner := string(storeref.RigRef(st.cfg.Rigs[0].Name))
	landed := seedGateBead(t, binding, prefix+"-owned", map[string]string{
		beadmeta.RootStoreRefMetadataKey: owner,
		beadmeta.WorkOutcomeMetadataKey:  beadmeta.WorkOutcomeShipped,
		beadmeta.WorkCommitMetadataKey:   commit,
		beadmeta.WorkBranchMetadataKey:   "main",
	})
	nowhere := seedGateBead(t, binding, prefix+"-nowhere", map[string]string{
		beadmeta.RootStoreRefMetadataKey: owner,
		beadmeta.WorkOutcomeMetadataKey:  beadmeta.WorkOutcomeShipped,
		beadmeta.WorkCommitMetadataKey:   "0000000000000000000000000000000000000000",
		beadmeta.WorkBranchMetadataKey:   "main",
	})

	t.Setenv(workrecord.EnforceEnvVar, "1")
	logged := captureWorkRecordGateLog(t)
	s := New(st)

	if _, err := s.humaHandleBeadClose(context.Background(), &BeadCloseInput{ID: landed}); err != nil {
		t.Fatalf("close of a rig-owned bead whose commit is on the rig checkout: %v (gate log: %s)", err, logged.String())
	}
	if out := logged.String(); strings.Contains(out, "reachability unverified") {
		t.Fatalf("gate output %q degraded a clause the rig checkout can answer", out)
	}
	closed, err := binding.Get(landed)
	if err != nil {
		t.Fatalf("Get(%s): %v", landed, err)
	}
	if closed.Status != "closed" {
		t.Fatalf("status = %q, want closed", closed.Status)
	}

	// The control: the same owner, a commit no checkout holds. Resolving the
	// repository correctly is not a license to stop asking.
	_, err = s.humaHandleBeadClose(context.Background(), &BeadCloseInput{ID: nowhere})
	assertConflict(t, err, "not reachable")
}
