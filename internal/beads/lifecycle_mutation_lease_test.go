package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

const (
	lifecycleMutationLeaseHelperFlag        = "--lifecycle-mutation-lease-helper"
	lifecycleMutationSiblingLeaseHelperFlag = "--lifecycle-mutation-sibling-lease-helper"
)

type lifecycleMutationLeaseAttempt struct {
	env map[string]string
	err error
}

func newLifecycleMutationLeaseScope(t *testing.T) string {
	t.Helper()
	scope := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scope, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.beads): %v", err)
	}
	return scope
}

func lifecycleMutationInheritanceFromCommandEnv(env map[string]string) (lifecycleMutationInheritance, error) {
	scope := env[lifecycleMutationScopeEnv]
	token := env[lifecycleMutationTokenEnv]
	if scope == "" || token == "" {
		return lifecycleMutationInheritance{}, fmt.Errorf(
			"lifecycle mutation command env missing scope or token: %#v", env)
	}
	return lifecycleMutationInheritance{scope: scope, token: token}, nil
}

func copyLifecycleMutationCommandEnv(env map[string]string) map[string]string {
	cloned := make(map[string]string, len(env))
	for key, value := range env {
		cloned[key] = value
	}
	return cloned
}

func assertLifecycleMutationChildEnv(t *testing.T, child, parent map[string]string) {
	t.Helper()
	if got, want := child[lifecycleMutationScopeEnv], parent[lifecycleMutationScopeEnv]; got != want {
		t.Fatalf("child lifecycle mutation scope = %q, want parent scope %q", got, want)
	}
	childDelegation, ok := parseLifecycleMutationDelegation(child[lifecycleMutationTokenEnv])
	if !ok {
		t.Fatalf("child lifecycle mutation token = %q, want valid delegation", child[lifecycleMutationTokenEnv])
	}
	parentDelegation, ok := parseLifecycleMutationDelegation(parent[lifecycleMutationTokenEnv])
	if !ok {
		t.Fatalf("parent lifecycle mutation token = %q, want valid delegation", parent[lifecycleMutationTokenEnv])
	}
	if childDelegation.rootToken != parentDelegation.rootToken {
		t.Fatalf("child lifecycle mutation root token = %q, want parent root token %q", childDelegation.rootToken, parentDelegation.rootToken)
	}
	if got, want := childDelegation.generation, parentDelegation.generation+1; got != want {
		t.Fatalf("child lifecycle mutation generation = %d, want %d", got, want)
	}
}

func startLifecycleMutationLeaseAttempt(scope string, inherited lifecycleMutationInheritance) (<-chan struct{}, <-chan lifecycleMutationLeaseAttempt) {
	started := make(chan struct{})
	done := make(chan lifecycleMutationLeaseAttempt, 1)
	go func() {
		close(started)
		lease, err := acquireLifecycleMutationLease(scope, inherited)
		if err != nil {
			done <- lifecycleMutationLeaseAttempt{err: err}
			return
		}
		env := copyLifecycleMutationCommandEnv(lease.CommandEnv())
		lease.Unlock()
		done <- lifecycleMutationLeaseAttempt{env: env}
	}()
	return started, done
}

// assertLifecycleMutationLeaseAttemptBlocked uses scheduler hand-offs instead
// of elapsed wall time. The contender has announced that its next operation is
// acquisition; repeated Gosched calls let it either return (a contract
// violation) or park on the held lease.
func assertLifecycleMutationLeaseAttemptBlocked(t *testing.T, done <-chan lifecycleMutationLeaseAttempt) {
	t.Helper()
	for range 1024 {
		select {
		case result := <-done:
			t.Fatalf("lifecycle mutation contender returned while owner held lease: err=%v env=%#v", result.err, result.env)
		default:
			runtime.Gosched()
		}
	}
}

func waitForLifecycleMutationEntryRefs(
	t *testing.T,
	entryKey string,
	want int,
	done <-chan lifecycleMutationLeaseAttempt,
) {
	t.Helper()
	deadline := time.NewTimer(testutil.GoroutineRaceTimeout)
	defer deadline.Stop()
	for {
		lifecycleMutationMutexRegistry.Lock()
		entry := lifecycleMutationMutexRegistry.entries[entryKey]
		got := 0
		if entry != nil {
			got = entry.refs
		}
		lifecycleMutationMutexRegistry.Unlock()
		if got >= want {
			return
		}
		select {
		case result := <-done:
			t.Fatalf("lifecycle mutation contender returned before waiting: err=%v env=%#v", result.err, result.env)
		case <-deadline.C:
			t.Fatalf("lifecycle mutation entry %q did not retain %d waiters", entryKey, want)
		default:
			runtime.Gosched()
		}
	}
}

func waitLifecycleMutationLeaseAttempt(t *testing.T, done <-chan lifecycleMutationLeaseAttempt) lifecycleMutationLeaseAttempt {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("lifecycle mutation lease attempt did not return")
		return lifecycleMutationLeaseAttempt{}
	}
}

func lifecycleMutationLeaseHelperArgs(args []string) (scope, resultPath string, ok bool) {
	if len(args) < 4 {
		return "", "", false
	}
	suffix := args[len(args)-4:]
	if suffix[0] != "--" || suffix[1] != lifecycleMutationLeaseHelperFlag {
		return "", "", false
	}
	if !filepath.IsAbs(suffix[2]) || filepath.Clean(suffix[2]) != suffix[2] {
		return "", "", false
	}
	if !filepath.IsAbs(suffix[3]) || filepath.Clean(suffix[3]) != suffix[3] {
		return "", "", false
	}
	return suffix[2], suffix[3], true
}

func runLifecycleMutationLeaseHelper(t *testing.T, scope, resultName string, inherited map[string]string) map[string]string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	resultPath := filepath.Join(scope, resultName)
	ctx, cancel := context.WithTimeout(context.Background(), testutil.ExecRaceTimeout)
	defer cancel()
	runner := ExecCommandEnvRunnerWithEnvContext(ctx, nil)
	out, err := runner(
		scope,
		executable,
		inherited,
		"-test.run=^TestLifecycleMutationLeaseHelperProcess$",
		"--",
		lifecycleMutationLeaseHelperFlag,
		scope,
		resultPath,
	)
	if err != nil {
		if ctx.Err() != nil {
			t.Fatalf("lifecycle mutation helper timed out while acquiring lease: %v; output=%s", ctx.Err(), out)
		}
		t.Fatalf("lifecycle mutation helper: %v; output=%s", err, out)
	}
	payload, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read lifecycle mutation helper result: %v; output=%s", err, out)
	}
	var commandEnv map[string]string
	if err := json.Unmarshal(payload, &commandEnv); err != nil {
		t.Fatalf("decode lifecycle mutation helper result %q: %v", payload, err)
	}
	return commandEnv
}

type lifecycleMutationSiblingHelper struct {
	cmd    *exec.Cmd
	done   <-chan error
	output *bytes.Buffer
}

func startLifecycleMutationSiblingHelper(
	t *testing.T,
	scope, acquiredPath, releasePath, nestedResultName string,
	inherited map[string]string,
) lifecycleMutationSiblingHelper {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testutil.ExecRaceTimeout)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(
		ctx,
		executable,
		"-test.run=^TestLifecycleMutationSiblingLeaseHelperProcess$",
		"--",
		lifecycleMutationSiblingLeaseHelperFlag,
		scope,
		acquiredPath,
		releasePath,
		nestedResultName,
	)
	baseEnv := envWithout(os.Environ(), lifecycleMutationScopeEnv)
	baseEnv = envWithout(baseEnv, lifecycleMutationTokenEnv)
	cmd.Env = execEnvFor(executable, baseEnv, inherited)
	cmd.Dir = scope
	output := &bytes.Buffer{}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start lifecycle mutation sibling helper: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	return lifecycleMutationSiblingHelper{cmd: cmd, done: done, output: output}
}

func waitForLifecycleMutationHelperFile(t *testing.T, path string, helper lifecycleMutationSiblingHelper) {
	t.Helper()
	deadline := time.NewTimer(testutil.ExecRaceTimeout)
	defer deadline.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat lifecycle mutation helper marker %s: %v", path, err)
		}
		select {
		case err := <-helper.done:
			t.Fatalf("lifecycle mutation helper exited before writing %s: %v; output=%s", path, err, helper.output)
		case <-deadline.C:
			t.Fatalf("timed out waiting for lifecycle mutation helper marker %s; output=%s", path, helper.output)
		default:
			runtime.Gosched()
		}
	}
}

func waitForLifecycleMutationSiblingHelper(t *testing.T, helper lifecycleMutationSiblingHelper) {
	t.Helper()
	select {
	case err := <-helper.done:
		if err != nil {
			t.Fatalf("lifecycle mutation sibling helper: %v; output=%s", err, helper.output)
		}
	case <-time.After(testutil.ExecRaceTimeout):
		_ = helper.cmd.Process.Kill()
		t.Fatalf("lifecycle mutation sibling helper timed out; output=%s", helper.output)
	}
}

// TestLifecycleMutationLeaseHelperProcess is re-executed by
// TestLifecycleMutationLeaseHelperProcessReentersHeldFlockAndRefreshesStaleToken.
// With no exact helper suffix it is an inert ordinary test.
func TestLifecycleMutationLeaseHelperProcess(t *testing.T) {
	scope, resultPath, ok := lifecycleMutationLeaseHelperArgs(os.Args)
	if !ok {
		return
	}
	lease, err := acquireLifecycleMutationLease(scope, inheritedLifecycleMutationFromEnv())
	if err != nil {
		t.Fatalf("helper acquire lifecycle mutation lease: %v", err)
	}
	defer lease.Unlock()
	payload, err := json.Marshal(lease.CommandEnv())
	if err != nil {
		t.Fatalf("helper encode command env: %v", err)
	}
	if err := os.WriteFile(resultPath, payload, 0o600); err != nil {
		t.Fatalf("helper write command env: %v", err)
	}
}

func lifecycleMutationSiblingLeaseHelperArgs(args []string) (
	scope, acquiredPath, releasePath, nestedResultName string,
	ok bool,
) {
	if len(args) < 6 {
		return "", "", "", "", false
	}
	suffix := args[len(args)-6:]
	if suffix[0] != "--" || suffix[1] != lifecycleMutationSiblingLeaseHelperFlag {
		return "", "", "", "", false
	}
	for _, candidate := range suffix[2:4] {
		if !filepath.IsAbs(candidate) || filepath.Clean(candidate) != candidate {
			return "", "", "", "", false
		}
	}
	if suffix[4] != "-" && (!filepath.IsAbs(suffix[4]) || filepath.Clean(suffix[4]) != suffix[4]) {
		return "", "", "", "", false
	}
	if suffix[5] != "-" && filepath.Base(suffix[5]) != suffix[5] {
		return "", "", "", "", false
	}
	return suffix[2], suffix[3], suffix[4], suffix[5], true
}

// TestLifecycleMutationSiblingLeaseHelperProcess is re-executed by
// TestLifecycleMutationLeaseSerializesSiblingProcessesAndAllowsNestedDescendant.
// With no exact helper suffix it is an inert ordinary test.
func TestLifecycleMutationSiblingLeaseHelperProcess(t *testing.T) {
	scope, acquiredPath, releasePath, nestedResultName, ok := lifecycleMutationSiblingLeaseHelperArgs(os.Args)
	if !ok {
		return
	}
	if err := os.WriteFile(acquiredPath+".attempting", []byte("attempting\n"), 0o600); err != nil {
		t.Fatalf("sibling helper write attempting marker: %v", err)
	}
	lease, err := acquireLifecycleMutationLease(scope, inheritedLifecycleMutationFromEnv())
	if err != nil {
		t.Fatalf("sibling helper acquire lifecycle mutation lease: %v", err)
	}
	defer lease.Unlock()
	if err := os.WriteFile(acquiredPath, []byte("acquired\n"), 0o600); err != nil {
		t.Fatalf("sibling helper write acquired marker: %v", err)
	}
	if nestedResultName != "-" {
		runLifecycleMutationLeaseHelper(t, scope, nestedResultName, lease.CommandEnv())
	}
	if releasePath == "-" {
		return
	}
	deadline := time.NewTimer(testutil.ExecRaceTimeout)
	defer deadline.Stop()
	for {
		if _, err := os.Stat(releasePath); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatalf("sibling helper stat release marker: %v", err)
		}
		select {
		case <-deadline.C:
			t.Fatalf("sibling helper timed out waiting for release marker %s", releasePath)
		default:
			runtime.Gosched()
		}
	}
}

func TestLifecycleMutationLeaseSerializesSiblingProcessesAndAllowsNestedDescendant(t *testing.T) {
	scope := newLifecycleMutationLeaseScope(t)
	owner, err := acquireLifecycleMutationLease(scope, lifecycleMutationInheritance{})
	if err != nil {
		t.Fatalf("acquire owner lease: %v", err)
	}
	defer owner.Unlock()

	ownerEnv := copyLifecycleMutationCommandEnv(owner.CommandEnv())
	firstAcquired := filepath.Join(scope, "first-sibling-acquired")
	firstRelease := filepath.Join(scope, "release-first-sibling")
	nestedResultName := "nested-descendant.json"
	first := startLifecycleMutationSiblingHelper(
		t,
		scope,
		firstAcquired,
		firstRelease,
		nestedResultName,
		ownerEnv,
	)
	waitForLifecycleMutationHelperFile(t, firstAcquired, first)
	waitForLifecycleMutationHelperFile(t, filepath.Join(scope, nestedResultName), first)

	secondAcquired := filepath.Join(scope, "second-sibling-acquired")
	second := startLifecycleMutationSiblingHelper(t, scope, secondAcquired, "-", "-", ownerEnv)
	waitForLifecycleMutationHelperFile(t, secondAcquired+".attempting", second)
	select {
	case err := <-second.done:
		t.Fatalf("second sibling entered while first sibling held inherited delegation: %v; output=%s", err, second.output)
	case <-time.After(100 * time.Millisecond):
		// The first helper owns this delegation generation. The sibling must be
		// parked on its cross-process generation lock, while the nested helper
		// above was free to enter the next generation.
	}

	if err := os.WriteFile(firstRelease, []byte("release\n"), 0o600); err != nil {
		t.Fatalf("release first sibling: %v", err)
	}
	waitForLifecycleMutationSiblingHelper(t, first)
	waitForLifecycleMutationSiblingHelper(t, second)
	if _, err := os.Stat(secondAcquired); err != nil {
		t.Fatalf("second sibling acquired marker after release: %v", err)
	}
}

func TestLifecycleMutationLeaseHelperProcessReentersHeldFlockAndRefreshesStaleToken(t *testing.T) {
	scope := newLifecycleMutationLeaseScope(t)
	owner, err := acquireLifecycleMutationLease(scope, lifecycleMutationInheritance{})
	if err != nil {
		t.Fatalf("acquire owner lease: %v", err)
	}
	ownerHeld := true
	defer func() {
		if ownerHeld {
			owner.Unlock()
		}
	}()

	ownerEnv := copyLifecycleMutationCommandEnv(owner.CommandEnv())
	matching := runLifecycleMutationLeaseHelper(t, scope, "matching-helper.json", ownerEnv)
	assertLifecycleMutationChildEnv(t, matching, ownerEnv)

	owner.Unlock()
	ownerHeld = false
	fresh := runLifecycleMutationLeaseHelper(t, scope, "stale-helper.json", ownerEnv)
	if got, want := fresh[lifecycleMutationScopeEnv], closeTransitionScopeKey(scope); got != want {
		t.Fatalf("stale child fresh scope = %q, want %q", got, want)
	}
	if got, stale := fresh[lifecycleMutationTokenEnv], ownerEnv[lifecycleMutationTokenEnv]; got == "" || got == stale {
		t.Fatalf("stale child fresh token = %q, want nonempty token different from released owner %q", got, stale)
	}
}

func TestLifecycleMutationLeaseReentersAcrossPhysicalScopeAlias(t *testing.T) {
	parent := t.TempDir()
	physicalScope := filepath.Join(parent, "physical")
	if err := os.MkdirAll(filepath.Join(physicalScope, ".beads"), 0o755); err != nil {
		t.Fatalf("create physical lifecycle scope: %v", err)
	}
	aliasScope := filepath.Join(parent, "alias")
	if err := os.Symlink(physicalScope, aliasScope); err != nil {
		t.Fatalf("symlink lifecycle scope: %v", err)
	}

	owner, err := acquireLifecycleMutationLease(aliasScope, lifecycleMutationInheritance{})
	if err != nil {
		t.Fatalf("acquire aliased owner lease: %v", err)
	}
	defer owner.Unlock()
	inherited, err := lifecycleMutationInheritanceFromCommandEnv(owner.CommandEnv())
	if err != nil {
		t.Fatal(err)
	}

	_, descendantDone := startLifecycleMutationLeaseAttempt(physicalScope, inherited)
	descendant := waitLifecycleMutationLeaseAttempt(t, descendantDone)
	if descendant.err != nil {
		t.Fatalf("physical-path descendant lease: %v", descendant.err)
	}
	assertLifecycleMutationChildEnv(t, descendant.env, owner.CommandEnv())
}

func TestLifecycleMutationScopeKeyBoundsEscapedLongOwnerRecord(t *testing.T) {
	scope := t.TempDir()
	escapedComponent := strings.Repeat("\x01", 200)
	for range 4 {
		scope = filepath.Join(scope, escapedComponent)
	}
	if err := os.MkdirAll(filepath.Join(scope, ".beads"), 0o755); err != nil {
		t.Fatalf("create escaped lifecycle scope: %v", err)
	}

	key := closeTransitionScopeKey(scope)
	if !strings.HasPrefix(key, "sha256:") || len(key) != len("sha256:")+64 {
		t.Fatalf("lifecycle scope key = %q (len %d), want fixed sha256 key", key, len(key))
	}
	record, err := json.Marshal(lifecycleMutationOwnerRecord{
		Scope: key,
		Token: strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatalf("marshal lifecycle owner record: %v", err)
	}
	if got := len(record); got > lifecycleMutationOwnerRecordMaxBytes {
		t.Fatalf("lifecycle owner record length = %d, max %d", got, lifecycleMutationOwnerRecordMaxBytes)
	}

	owner, err := acquireLifecycleMutationLease(scope, lifecycleMutationInheritance{})
	if err != nil {
		t.Fatalf("acquire escaped-path owner lease: %v", err)
	}
	defer owner.Unlock()
	inherited, err := lifecycleMutationInheritanceFromCommandEnv(owner.CommandEnv())
	if err != nil {
		t.Fatal(err)
	}
	_, descendantDone := startLifecycleMutationLeaseAttempt(scope, inherited)
	descendant := waitLifecycleMutationLeaseAttempt(t, descendantDone)
	if descendant.err != nil {
		t.Fatalf("escaped-path descendant lease: %v", descendant.err)
	}
}

func TestLifecycleMutationLeasePreservesWhitespaceInFilesystemScope(t *testing.T) {
	scope := filepath.Join(t.TempDir(), " city scope ")
	if err := os.MkdirAll(filepath.Join(scope, ".beads"), 0o755); err != nil {
		t.Fatalf("create whitespace lifecycle scope: %v", err)
	}

	lease, err := acquireLifecycleMutationLease(scope, lifecycleMutationInheritance{})
	if err != nil {
		t.Fatalf("acquire whitespace-path lease: %v", err)
	}
	lease.Unlock()
	lockPath := filepath.Join(scope, ".beads", lifecycleMutationLockFilename)
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("whitespace-path lifecycle owner record %s: %v", lockPath, err)
	}
	if got, trimmed := closeTransitionScopeKey(scope), closeTransitionScopeKey(strings.TrimSpace(scope)); got == trimmed {
		t.Fatalf("whitespace scope key %q aliases trimmed path %q", got, strings.TrimSpace(scope))
	}
}

func TestLifecycleMutationLeaseMatchingInheritedTokenReentersImmediately(t *testing.T) {
	scope := newLifecycleMutationLeaseScope(t)
	owner, err := acquireLifecycleMutationLease(scope, lifecycleMutationInheritance{})
	if err != nil {
		t.Fatalf("acquire owner lease: %v", err)
	}
	ownerHeld := true
	defer func() {
		if ownerHeld {
			owner.Unlock()
		}
	}()

	lockPath := filepath.Join(scope, ".beads", lifecycleMutationLockFilename)
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("unified lifecycle mutation lock %s: %v", lockPath, err)
	}

	ownerEnv := copyLifecycleMutationCommandEnv(owner.CommandEnv())
	inherited, err := lifecycleMutationInheritanceFromCommandEnv(ownerEnv)
	if err != nil {
		t.Fatal(err)
	}
	_, nestedDone := startLifecycleMutationLeaseAttempt(scope, inherited)
	nested := waitLifecycleMutationLeaseAttempt(t, nestedDone)
	if nested.err != nil {
		t.Fatalf("matching inherited lease: %v", nested.err)
	}
	assertLifecycleMutationChildEnv(t, nested.env, ownerEnv)

	// Releasing a reentrant handle must not release the actual owner. A fresh
	// caller still waits until the outermost owner exits.
	started, contenderDone := startLifecycleMutationLeaseAttempt(scope, lifecycleMutationInheritance{})
	<-started
	assertLifecycleMutationLeaseAttemptBlocked(t, contenderDone)
	owner.Unlock()
	ownerHeld = false
	contender := waitLifecycleMutationLeaseAttempt(t, contenderDone)
	if contender.err != nil {
		t.Fatalf("fresh contender after owner release: %v", contender.err)
	}
}

func TestLifecycleMutationLeaseMismatchedInheritedTokenBlocks(t *testing.T) {
	scope := newLifecycleMutationLeaseScope(t)
	owner, err := acquireLifecycleMutationLease(scope, lifecycleMutationInheritance{})
	if err != nil {
		t.Fatalf("acquire owner lease: %v", err)
	}
	ownerHeld := true
	defer func() {
		if ownerHeld {
			owner.Unlock()
		}
	}()

	ownerInheritance, err := lifecycleMutationInheritanceFromCommandEnv(owner.CommandEnv())
	if err != nil {
		t.Fatal(err)
	}
	mismatched := lifecycleMutationInheritance{
		scope: ownerInheritance.scope,
		token: "not-" + ownerInheritance.token,
	}
	started, contenderDone := startLifecycleMutationLeaseAttempt(scope, mismatched)
	<-started
	assertLifecycleMutationLeaseAttemptBlocked(t, contenderDone)

	owner.Unlock()
	ownerHeld = false
	contender := waitLifecycleMutationLeaseAttempt(t, contenderDone)
	if contender.err != nil {
		t.Fatalf("mismatched-token contender after owner release: %v", contender.err)
	}
	if contender.env[lifecycleMutationTokenEnv] == mismatched.token {
		t.Fatalf("mismatched inherited token %q was trusted as the new owner", mismatched.token)
	}
}

func TestLifecycleMutationLeaseStaleTokenAfterOwnerExitAcquiresNormally(t *testing.T) {
	scope := newLifecycleMutationLeaseScope(t)
	owner, err := acquireLifecycleMutationLease(scope, lifecycleMutationInheritance{})
	if err != nil {
		t.Fatalf("acquire owner lease: %v", err)
	}
	stale, err := lifecycleMutationInheritanceFromCommandEnv(owner.CommandEnv())
	if err != nil {
		owner.Unlock()
		t.Fatal(err)
	}
	owner.Unlock()

	_, acquired := startLifecycleMutationLeaseAttempt(scope, stale)
	fresh := waitLifecycleMutationLeaseAttempt(t, acquired)
	if fresh.err != nil {
		t.Fatalf("acquire with stale inheritance after owner exit: %v", fresh.err)
	}
	if got := fresh.env[lifecycleMutationScopeEnv]; got != closeTransitionScopeKey(scope) {
		t.Fatalf("fresh lease scope = %q, want %q", got, closeTransitionScopeKey(scope))
	}
	if got := fresh.env[lifecycleMutationTokenEnv]; got == "" || got == stale.token {
		t.Fatalf("fresh lease token = %q, want a nonempty token different from stale owner %q", got, stale.token)
	}
}

func TestLifecycleMutationLeaseStaleSiblingFallsBackAfterOwnerReplacement(t *testing.T) {
	scope := newLifecycleMutationLeaseScope(t)
	original, err := acquireLifecycleMutationLease(scope, lifecycleMutationInheritance{})
	if err != nil {
		t.Fatalf("acquire original owner lease: %v", err)
	}
	originalHeld := true
	defer func() {
		if originalHeld {
			original.Unlock()
		}
	}()

	stale, err := lifecycleMutationInheritanceFromCommandEnv(original.CommandEnv())
	if err != nil {
		t.Fatal(err)
	}
	firstSibling, err := acquireLifecycleMutationLease(scope, stale)
	if err != nil {
		t.Fatalf("acquire first sibling lease: %v", err)
	}
	firstSiblingHeld := true
	defer func() {
		if firstSiblingHeld {
			firstSibling.Unlock()
		}
	}()

	_, secondDone := startLifecycleMutationLeaseAttempt(scope, stale)
	entryKey := lifecycleMutationEntryKey(closeTransitionScopeKey(scope), 1)
	waitForLifecycleMutationEntryRefs(t, entryKey, 2, secondDone)

	original.Unlock()
	originalHeld = false
	replacement, err := acquireLifecycleMutationLease(scope, lifecycleMutationInheritance{})
	if err != nil {
		t.Fatalf("acquire replacement owner lease: %v", err)
	}
	replacementHeld := true
	defer func() {
		if replacementHeld {
			replacement.Unlock()
		}
	}()

	firstSibling.Unlock()
	firstSiblingHeld = false
	assertLifecycleMutationLeaseAttemptBlocked(t, secondDone)

	replacement.Unlock()
	replacementHeld = false
	second := waitLifecycleMutationLeaseAttempt(t, secondDone)
	if second.err != nil {
		t.Fatalf("stale sibling after owner replacement: %v", second.err)
	}
	oldDelegation, ok := parseLifecycleMutationDelegation(stale.token)
	if !ok {
		t.Fatalf("original delegation token = %q, want valid delegation", stale.token)
	}
	newDelegation, ok := parseLifecycleMutationDelegation(second.env[lifecycleMutationTokenEnv])
	if !ok {
		t.Fatalf("replacement delegation token = %q, want valid delegation", second.env[lifecycleMutationTokenEnv])
	}
	if newDelegation.rootToken == oldDelegation.rootToken {
		t.Fatalf("replacement root token = %q, want a fresh root", newDelegation.rootToken)
	}
}

func TestLifecycleMutationLeaseEmptyOwnerFenceFallsBackFresh(t *testing.T) {
	scope := newLifecycleMutationLeaseScope(t)
	root, err := openLifecycleMutationScopeLock(scope)
	if err != nil {
		t.Fatalf("open root lifecycle lock: %v", err)
	}
	if root == nil {
		t.Fatal("open root lifecycle lock returned nil")
	}
	rootHeld := true
	defer func() {
		if rootHeld {
			_ = root.Unlock()
		}
	}()
	if err := root.Lock(); err != nil {
		t.Fatalf("lock root lifecycle lock: %v", err)
	}
	if err := root.WriteLocked(nil); err != nil {
		t.Fatalf("write empty owner fence: %v", err)
	}

	staleRoot := strings.Repeat("a", 64)
	stale := lifecycleMutationInheritance{
		scope: closeTransitionScopeKey(scope),
		token: encodeLifecycleMutationDelegation(staleRoot, 1),
	}
	started, done := startLifecycleMutationLeaseAttempt(scope, stale)
	<-started
	assertLifecycleMutationLeaseAttemptBlocked(t, done)

	if err := root.Unlock(); err != nil {
		t.Fatalf("unlock root lifecycle lock: %v", err)
	}
	rootHeld = false
	assertLifecycleMutationLeaseAttemptReturnsFreshRoot(t, done, staleRoot)
}

func TestLifecycleMutationLeaseStaleRecordWithoutHeldProofFallsBackFresh(t *testing.T) {
	scope := newLifecycleMutationLeaseScope(t)
	root, err := openLifecycleMutationScopeLock(scope)
	if err != nil {
		t.Fatalf("open root lifecycle lock: %v", err)
	}
	if root == nil {
		t.Fatal("open root lifecycle lock returned nil")
	}
	rootHeld := true
	defer func() {
		if rootHeld {
			_ = root.Unlock()
		}
	}()
	if err := root.Lock(); err != nil {
		t.Fatalf("lock root lifecycle lock: %v", err)
	}

	staleRoot := strings.Repeat("b", 64)
	record, err := json.Marshal(lifecycleMutationOwnerRecord{
		Scope: closeTransitionScopeKey(scope),
		Token: staleRoot,
	})
	if err != nil {
		t.Fatalf("marshal stale owner record: %v", err)
	}
	if err := root.WriteLocked(append(record, '\n')); err != nil {
		t.Fatalf("write stale owner record: %v", err)
	}

	stale := lifecycleMutationInheritance{
		scope: closeTransitionScopeKey(scope),
		token: encodeLifecycleMutationDelegation(staleRoot, 1),
	}
	started, done := startLifecycleMutationLeaseAttempt(scope, stale)
	<-started
	assertLifecycleMutationLeaseAttemptBlocked(t, done)

	if err := root.Unlock(); err != nil {
		t.Fatalf("unlock root lifecycle lock: %v", err)
	}
	rootHeld = false
	assertLifecycleMutationLeaseAttemptReturnsFreshRoot(t, done, staleRoot)
}

func TestLifecycleMutationLeaseRemovesOwnerProofOnUnlock(t *testing.T) {
	scope := newLifecycleMutationLeaseScope(t)
	owner, err := acquireLifecycleMutationLease(scope, lifecycleMutationInheritance{})
	if err != nil {
		t.Fatalf("acquire lifecycle mutation owner: %v", err)
	}
	delegation, ok := parseLifecycleMutationDelegation(owner.CommandEnv()[lifecycleMutationTokenEnv])
	if !ok {
		owner.Unlock()
		t.Fatal("owner command environment did not contain a valid delegation")
	}
	proofPath := filepath.Join(
		scope,
		".beads",
		lifecycleMutationOwnerProofLockFilename(delegation.rootToken),
	)
	if _, err := os.Stat(proofPath); err != nil {
		owner.Unlock()
		t.Fatalf("stat held owner proof: %v", err)
	}

	owner.Unlock()
	if _, err := os.Stat(proofPath); !os.IsNotExist(err) {
		t.Fatalf("owner proof after unlock: stat err=%v, want not exist", err)
	}
}

func assertLifecycleMutationLeaseAttemptReturnsFreshRoot(
	t *testing.T,
	done <-chan lifecycleMutationLeaseAttempt,
	staleRoot string,
) {
	t.Helper()
	result := waitLifecycleMutationLeaseAttempt(t, done)
	if result.err != nil {
		t.Fatalf("fresh lifecycle mutation fallback: %v", result.err)
	}
	delegation, ok := parseLifecycleMutationDelegation(result.env[lifecycleMutationTokenEnv])
	if !ok {
		t.Fatalf("fresh lifecycle mutation delegation = %q, want valid token", result.env[lifecycleMutationTokenEnv])
	}
	if delegation.rootToken == staleRoot {
		t.Fatalf("fresh lifecycle mutation root token = %q, want a token different from stale root", delegation.rootToken)
	}
}

func TestLifecycleMutationLeaseQueuedDescendantFailsClosedOnUnreadableOwnerRecord(t *testing.T) {
	scope := newLifecycleMutationLeaseScope(t)
	owner, err := acquireLifecycleMutationLease(scope, lifecycleMutationInheritance{})
	if err != nil {
		t.Fatalf("acquire lifecycle mutation owner: %v", err)
	}
	defer owner.Unlock()

	inherited, err := lifecycleMutationInheritanceFromCommandEnv(owner.CommandEnv())
	if err != nil {
		t.Fatal(err)
	}
	delegation, ok := parseLifecycleMutationDelegation(inherited.token)
	if !ok {
		t.Fatalf("owner delegation token = %q, want valid delegation", inherited.token)
	}
	firstSibling, err := acquireLifecycleMutationLease(scope, inherited)
	if err != nil {
		t.Fatalf("acquire first sibling lease: %v", err)
	}
	firstSiblingHeld := true
	defer func() {
		if firstSiblingHeld {
			firstSibling.Unlock()
		}
	}()

	_, queuedDone := startLifecycleMutationLeaseAttempt(scope, inherited)
	entryKey := lifecycleMutationEntryKey(closeTransitionScopeKey(scope), delegation.generation)
	waitForLifecycleMutationEntryRefs(t, entryKey, 2, queuedDone)

	ownerRecordPath := filepath.Join(scope, ".beads", lifecycleMutationLockFilename)
	if err := os.WriteFile(ownerRecordPath, []byte("{invalid owner record\n"), 0o600); err != nil {
		t.Fatalf("corrupt lifecycle mutation owner record: %v", err)
	}
	firstSibling.Unlock()
	firstSiblingHeld = false

	queued := waitLifecycleMutationLeaseAttempt(t, queuedDone)
	if queued.err == nil {
		t.Fatal("queued descendant trusted an unreadable lifecycle mutation owner record")
	}
}

func TestLifecycleMutationLeaseProcessOnlyStaleSiblingFallsBackAfterOwnerReplacement(t *testing.T) {
	scope := t.TempDir()
	original, err := acquireLifecycleMutationLease(scope, lifecycleMutationInheritance{})
	if err != nil {
		t.Fatalf("acquire original owner lease: %v", err)
	}
	originalHeld := true
	defer func() {
		if originalHeld {
			original.Unlock()
		}
	}()

	stale, err := lifecycleMutationInheritanceFromCommandEnv(original.CommandEnv())
	if err != nil {
		t.Fatal(err)
	}
	firstSibling, err := acquireLifecycleMutationLease(scope, stale)
	if err != nil {
		t.Fatalf("acquire first sibling lease: %v", err)
	}
	firstSiblingHeld := true
	defer func() {
		if firstSiblingHeld {
			firstSibling.Unlock()
		}
	}()

	_, secondDone := startLifecycleMutationLeaseAttempt(scope, stale)
	entryKey := lifecycleMutationEntryKey(closeTransitionScopeKey(scope), 1)
	waitForLifecycleMutationEntryRefs(t, entryKey, 2, secondDone)

	original.Unlock()
	originalHeld = false
	replacement, err := acquireLifecycleMutationLease(scope, lifecycleMutationInheritance{})
	if err != nil {
		t.Fatalf("acquire replacement owner lease: %v", err)
	}
	replacementHeld := true
	defer func() {
		if replacementHeld {
			replacement.Unlock()
		}
	}()

	firstSibling.Unlock()
	firstSiblingHeld = false
	assertLifecycleMutationLeaseAttemptBlocked(t, secondDone)

	replacement.Unlock()
	replacementHeld = false
	second := waitLifecycleMutationLeaseAttempt(t, secondDone)
	if second.err != nil {
		t.Fatalf("process-only stale sibling after owner replacement: %v", second.err)
	}
	oldDelegation, ok := parseLifecycleMutationDelegation(stale.token)
	if !ok {
		t.Fatalf("original delegation token = %q, want valid delegation", stale.token)
	}
	newDelegation, ok := parseLifecycleMutationDelegation(second.env[lifecycleMutationTokenEnv])
	if !ok {
		t.Fatalf("replacement delegation token = %q, want valid delegation", second.env[lifecycleMutationTokenEnv])
	}
	if newDelegation.rootToken == oldDelegation.rootToken {
		t.Fatalf("replacement root token = %q, want a fresh root", newDelegation.rootToken)
	}
}

func TestLifecycleMutationLeaseSynchronousBDHooksCloseUpdateAndReenterWithoutABBA(t *testing.T) {
	scope := newLifecycleMutationLeaseScope(t)
	owner, err := acquireLifecycleMutationLease(scope, lifecycleMutationInheritance{})
	if err != nil {
		t.Fatalf("acquire owner lease: %v", err)
	}
	defer owner.Unlock()

	trace := make([]string, 0, 5)
	var runner CommandEnvRunner
	runMutation := func(inheritedEnv map[string]string, verb, id string) error {
		inherited, err := lifecycleMutationInheritanceFromCommandEnv(inheritedEnv)
		if err != nil {
			return err
		}
		lease, err := acquireLifecycleMutationLease(scope, inherited)
		if err != nil {
			return err
		}
		defer lease.Unlock()
		_, err = runner(scope, "bd", lease.CommandEnv(), verb, id)
		return err
	}
	runner = func(_ string, name string, env map[string]string, args ...string) ([]byte, error) {
		if name != "bd" || len(args) != 2 {
			return nil, fmt.Errorf("unexpected command %s %q", name, args)
		}
		verb, id := args[0], args[1]
		trace = append(trace, verb+":"+id)
		switch verb {
		case "close":
			if id != "source" {
				return nil, nil
			}
			trace = append(trace, "hook:on_close")
			if err := runMutation(env, "close", "root"); err != nil {
				return nil, fmt.Errorf("on_close nested close: %w", err)
			}
			if err := runMutation(env, "update", "root"); err != nil {
				return nil, fmt.Errorf("on_close nested update: %w", err)
			}
			return nil, nil
		case "update":
			trace = append(trace, "hook:on_update")
			inherited, err := lifecycleMutationInheritanceFromCommandEnv(env)
			if err != nil {
				return nil, err
			}
			lease, err := acquireLifecycleMutationLease(scope, inherited)
			if err != nil {
				return nil, fmt.Errorf("on_update lifecycle mutation: %w", err)
			}
			lease.Unlock()
			trace = append(trace, "lifecycle:entered")
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected bd verb %q", verb)
		}
	}

	done := make(chan error, 1)
	go func() {
		_, err := runner(scope, "bd", owner.CommandEnv(), "close", "source")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("synchronous hook chain: %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("synchronous on_close/on_update lifecycle mutation deadlocked")
	}

	wantTrace := []string{
		"close:source",
		"hook:on_close",
		"close:root",
		"update:root",
		"hook:on_update",
		"lifecycle:entered",
	}
	if !reflect.DeepEqual(trace, wantTrace) {
		t.Fatalf("hook trace = %v, want %v", trace, wantTrace)
	}
}
