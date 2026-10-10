package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// `gc hook --claim` on a work store served by a REMOTE beads backend (DESIGN
// C5 G8).
//
// Every claim-cycle op in hookClaimOps defaults to a bd subprocess
// (hookClaimWithBdStore and its siblings). For a remote scope that is the
// wrong lane under beads.native_transport = "auto": the native store is
// REQUIRED there (decideNativeTransport), so the claim must go through the
// native Claimer over the wire, with the scope's own credential, and a store
// that cannot open must fail the claim with the terminal
// *HTTPNativeOpenRequiredError rather than slip onto the bd CLI. Under
// native_transport = "off" (the rollback lever) the bd CLI is the lane, and
// its child must carry the scope's per-scope credential the way every other
// BdStore child of that scope does (G7), or it would fall back to the
// machine-wide credential ladder. The hook's work query is such a child too
// (bd ready against the remote scope, on either lane), so hookWorkQueryRunner
// projects the same credential into it.
//
// remoteRoutedHookClaimOps decides that PER CALL: one federated claim walks
// several legs (rig store, city store) under one ops value, and only a leg
// whose scope is remote changes. A local leg runs the exact function it ran
// before, with the exact env. The lane itself is the factory's verdict, not a
// second reading of native_transport: the leg's scope is opened through the
// same store open every other command uses, and a NativeDoltStore answer is
// the native lane while a BdStore answer is the CLI lane.
//
// A wisp the remote work store holds is refused BY NAME on either lane
// (*beads.WispClaimRefusedError, ErrWispNotClaimable): gc claims its wisps in
// the graph store, and a bare not-found would send the class route looking
// for the wisp there instead (see beads/remote_wisp_claim.go).

// hookClaimRemoteNativeStore is what the native lane calls on a remote work
// store: the raw *beads.NativeDoltStore surface, without the policy wrapper
// (the bd CLI lane applies no gc bead policy either).
type hookClaimRemoteNativeStore interface {
	Claim(id, assignee string) (beads.Bead, bool, error)
	Get(id string) (beads.Bead, error)
	List(query beads.ListQuery) ([]beads.Bead, error)
	Update(id string, opts beads.UpdateOpts) error
	ReleaseIfCurrent(id, expectedAssignee string) (bool, error)
	TransferIfCurrent(id, fromAssignee, toAssignee string) (bool, error)
}

// hookClaimRemoteLane is one remote scope's lane for this invocation: the
// native store, or nil for the bd CLI.
type hookClaimRemoteLane struct {
	native hookClaimRemoteNativeStore
}

// hookClaimRemoteRouter holds the per-invocation decisions. Its seams default
// to production and are replaced by tests.
type hookClaimRemoteRouter struct {
	cityPath string
	stderr   io.Writer
	// isRemote reports whether a scope's work store is served by a registered
	// remote backend.
	isRemote func(scopeRoot string) bool
	// openLane opens the scope through the ordinary store open and reports
	// the lane it selected. An error is the terminal open refusal.
	openLane func(scopeRoot string) (hookClaimRemoteLane, func(), error)
	// credentialEnv projects the scope's per-scope credential into a bd
	// child's env (unchanged when none is configured).
	credentialEnv func(scopeRoot string, env []string) ([]string, error)

	lanes         map[string]hookClaimRemoteLane
	closers       []func()
	warnedReclaim bool
}

func newHookClaimRemoteRouter(cityPath string, stderr io.Writer) *hookClaimRemoteRouter {
	r := &hookClaimRemoteRouter{cityPath: cityPath, stderr: stderr, lanes: map[string]hookClaimRemoteLane{}}
	r.isRemote = func(scopeRoot string) bool {
		_, remote := beads.RemoteBackendActivationRoot(scopeRoot, cityPath)
		return remote
	}
	r.openLane = func(scopeRoot string) (hookClaimRemoteLane, func(), error) {
		return openHookClaimRemoteLane(scopeRoot, cityPath)
	}
	r.credentialEnv = func(scopeRoot string, env []string) ([]string, error) {
		return hookClaimEnvWithScopeCredential(cityPath, scopeRoot, env)
	}
	return r
}

// openHookClaimRemoteLane opens scopeRoot through the shared store open (so
// native_transport, the deprecated force-fallback alias, the wire_compat
// preflight and the per-scope credential all decide exactly as they do for
// every other command) and reads the lane off the factory's diagnostic.
func openHookClaimRemoteLane(scopeRoot, cityPath string) (hookClaimRemoteLane, func(), error) {
	result, err := openStoreResultAtForCity(scopeRoot, cityPath)
	if err != nil {
		return hookClaimRemoteLane{}, nil, err
	}
	closeStore := func() {
		if closer, ok := rawHookClaimStore(result.Store).(interface{ CloseStore() error }); ok {
			_ = closer.CloseStore() //nolint:errcheck // best-effort shutdown at hook exit
		}
	}
	if result.Diagnostic.Store != beads.BeadsStoreNameNativeDoltStore {
		closeStore()
		return hookClaimRemoteLane{}, nil, nil
	}
	native, ok := rawHookClaimStore(result.Store).(hookClaimRemoteNativeStore)
	if !ok {
		closeStore()
		return hookClaimRemoteLane{}, nil, fmt.Errorf("remote work store at %s opened as %T, which cannot serve the hook's claim cycle", scopeRoot, result.Store)
	}
	return hookClaimRemoteLane{native: native}, closeStore, nil
}

// rawHookClaimStore returns the store beneath cmd/gc's bead-policy wrapper,
// which forwards Claim but not TransferIfCurrent.
func rawHookClaimStore(store beads.Store) beads.Store {
	raw, _, _ := unwrapBeadPolicyStore(store)
	return raw
}

// hookClaimEnvWithScopeCredential is env with the scope's per-scope credential
// projected (beads.RemoteCredentialSubprocessEnv): BEADS_HTTP_TOKEN for this
// child only, the ambient token command and CA blanked. A scope with no
// configured credential keeps env unchanged (bd's own ladder).
func hookClaimEnvWithScopeCredential(cityPath, scopeRoot string, env []string) ([]string, error) {
	projected := map[string]string{}
	applied, err := beads.NewRemoteCredentialSubprocessEnv(cityPath, scopeRoot).Apply(context.Background(), projected)
	if err != nil || !applied {
		return env, err
	}
	if env == nil {
		// A nil leg env means "inherit" (workQueryEnvForDir); keep that
		// meaning rather than handing bd an env of the token alone.
		env = mergeRuntimeEnv(os.Environ(), nil)
	}
	out := append([]string(nil), env...)
	keys := make([]string, 0, len(projected))
	for key := range projected {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out = append(removeEnvKey(out, key), key+"="+projected[key])
	}
	return out, nil
}

// hookClaimRemoteScopeRoot is the scope a leg's work store lives in: the
// parent of the leg's own BEADS_DIR (the value bd itself resolves the store
// from; every setter writes filepath.Join(root, ".beads")), else dir. A nil
// env inherits the process environment, so its BEADS_DIR is the process's.
//
// One exception keeps the scope a RIG's: a rig with no metadata of its own
// under a remote city reaches the city's server, so its leg's BEADS_DIR names
// the CITY's activation (bd_env.go), but its credential is the rig's own
// beads_credential when it has one. Its leg also carries GC_STORE_ROOT (the
// work-query env's store scope), and that scope wins when its activation IS
// the one BEADS_DIR names. A GC_STORE_ROOT that does not share the activation
// (a variable inherited from an agent's own scope) is ignored.
func hookClaimRemoteScopeRoot(cityPath, dir string, env []string) string {
	if env == nil {
		env = os.Environ()
	}
	beadsDir := hookClaimEnvValue(env, "BEADS_DIR")
	if beadsDir == "" || filepath.Base(filepath.Clean(beadsDir)) != ".beads" {
		return dir
	}
	root := filepath.Dir(filepath.Clean(beadsDir))
	if root == "" || root == "." {
		return dir
	}
	if store := hookClaimEnvValue(env, "GC_STORE_ROOT"); store != "" && filepath.IsAbs(store) && !samePath(store, root) {
		if activation, remote := beads.RemoteBackendActivationRoot(store, cityPath); remote && samePath(activation, root) {
			return filepath.Clean(store)
		}
	}
	return root
}

// hookWorkQueryRunner is the hook's work-query runner. A work query (bd
// ready, or gc ready's bd legs) is a bd child of its leg's store like any
// other, so a leg whose scope is served by a registered remote backend gets
// the scope's per-scope credential projected exactly the way gc bd's and
// BdStore's children get it: BEADS_HTTP_TOKEN=host:port=token in this child's
// env only, from the scope's shared provider, the ambient token command and
// CA blanked, and never on argv (the command string is unchanged). This holds
// on either claim lane: the work query runs before the lane is chosen. A
// scope with no configured credential, and every local leg, runs exactly as
// before.
func hookWorkQueryRunner(cityPath string) hookStoreRunner {
	return func(command, dir string, env []string) (string, error) {
		scope := hookClaimRemoteScopeRoot(cityPath, dir, env)
		if _, remote := beads.RemoteBackendActivationRoot(scope, cityPath); remote {
			projected, err := hookClaimEnvWithScopeCredential(cityPath, scope, env)
			if err != nil {
				return "", fmt.Errorf("running the work query against the remote work store at %s: %w", scope, err)
			}
			env = projected
		}
		return shellWorkQueryWithEnv(command, dir, env)
	}
}

// leg resolves one call's leg. remote is false for a local leg, which the
// caller hands to the base op untouched.
func (r *hookClaimRemoteRouter) leg(dir string, env []string) (lane hookClaimRemoteLane, scope string, legEnv []string, remote bool, err error) {
	scope = hookClaimRemoteScopeRoot(r.cityPath, dir, env)
	if !r.isRemote(scope) {
		return hookClaimRemoteLane{}, scope, env, false, nil
	}
	legEnv, err = r.credentialEnv(scope, env)
	if err != nil {
		return hookClaimRemoteLane{}, scope, env, true, err
	}
	if cached, ok := r.lanes[scope]; ok {
		return cached, scope, legEnv, true, nil
	}
	lane, closeLane, err := r.openLane(scope)
	if err != nil {
		return hookClaimRemoteLane{}, scope, legEnv, true, fmt.Errorf("opening the remote work store at %s: %w", scope, err)
	}
	if closeLane != nil {
		r.closers = append(r.closers, closeLane)
	}
	r.lanes[scope] = lane
	return lane, scope, legEnv, true, nil
}

// Close closes every native store this invocation opened.
func (r *hookClaimRemoteRouter) Close() {
	for _, closeLane := range r.closers {
		closeLane()
	}
	r.closers = nil
}

// remoteRoutedHookClaimOps returns ops whose with-env claim-cycle fields
// route remote legs (see the file comment), plus the cleanup the caller
// defers. Fields the router does not need are left to the base.
func remoteRoutedHookClaimOps(ops hookClaimOps, router *hookClaimRemoteRouter) hookClaimOps {
	ops.applyDefaults()
	base := ops

	ops.Claim = func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
		lane, scope, legEnv, remote, err := router.leg(dir, env)
		switch {
		case !remote:
			return base.Claim(ctx, dir, env, beadID, assignee)
		case err != nil:
			return beads.Bead{}, false, err
		case lane.native == nil:
			return base.Claim(ctx, dir, legEnv, beadID, assignee)
		}
		return hookClaimThroughStore(beadID, assignee, func() (beads.Bead, bool, error) {
			claimed, ok, claimErr := lane.native.Claim(beadID, assignee)
			var named *beads.WispClaimRefusedError
			if errors.Is(claimErr, beads.ErrWispNotClaimable) && !errors.As(claimErr, &named) {
				claimErr = &beads.WispClaimRefusedError{ID: beadID, ScopeRoot: scope, Detail: claimErr.Error()}
			}
			return claimed, ok, claimErr
		}, lane.native.Get)
	}
	ops.ListContinuation = func(ctx context.Context, dir string, env []string, rootID, group string) ([]beads.Bead, error) {
		lane, _, legEnv, remote, err := router.leg(dir, env)
		switch {
		case !remote:
			return base.ListContinuation(ctx, dir, env, rootID, group)
		case err != nil:
			return nil, err
		case lane.native == nil:
			return base.ListContinuation(ctx, dir, legEnv, rootID, group)
		}
		return lane.native.List(beads.ListQuery{
			Status: "open",
			Metadata: map[string]string{
				beadmeta.RootBeadIDMetadataKey:        rootID,
				beadmeta.ContinuationGroupMetadataKey: group,
			},
			TierMode: beads.TierBoth,
		})
	}
	ops.AssignContinuation = func(ctx context.Context, dir string, env []string, beadID, assignee string) error {
		lane, _, legEnv, remote, err := router.leg(dir, env)
		switch {
		case !remote:
			return base.AssignContinuation(ctx, dir, env, beadID, assignee)
		case err != nil:
			return err
		case lane.native == nil:
			return base.AssignContinuation(ctx, dir, legEnv, beadID, assignee)
		}
		return lane.native.Update(beadID, beads.UpdateOpts{Assignee: &assignee})
	}
	ops.StampWorkMeta = func(ctx context.Context, dir string, env []string, beadID, assignee string, patch map[string]string) error {
		lane, _, legEnv, remote, err := router.leg(dir, env)
		switch {
		case !remote:
			return base.StampWorkMeta(ctx, dir, env, beadID, assignee, patch)
		case err != nil:
			return err
		case lane.native == nil:
			return base.StampWorkMeta(ctx, dir, legEnv, beadID, assignee, patch)
		}
		return lane.native.Update(beadID, beads.UpdateOpts{Metadata: patch})
	}
	ops.ReadWorkMeta = func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, error) {
		lane, _, legEnv, remote, err := router.leg(dir, env)
		switch {
		case !remote:
			return base.ReadWorkMeta(ctx, dir, env, beadID, assignee)
		case err != nil:
			return beads.Bead{}, err
		case lane.native == nil:
			return base.ReadWorkMeta(ctx, dir, legEnv, beadID, assignee)
		}
		return lane.native.Get(beadID)
	}
	ops.Release = func(ctx context.Context, dir string, env []string, beadID, assignee string) (bool, error) {
		lane, _, legEnv, remote, err := router.leg(dir, env)
		switch {
		case !remote:
			return base.Release(ctx, dir, env, beadID, assignee)
		case err != nil:
			return false, err
		case lane.native == nil:
			return base.Release(ctx, dir, legEnv, beadID, assignee)
		}
		return lane.native.ReleaseIfCurrent(beadID, assignee)
	}
	ops.RestampAdopted = func(ctx context.Context, dir string, env []string, beadID, fromAssignee, toAssignee string) (bool, error) {
		lane, _, legEnv, remote, err := router.leg(dir, env)
		switch {
		case !remote:
			return base.RestampAdopted(ctx, dir, env, beadID, fromAssignee, toAssignee)
		case err != nil:
			return false, err
		case lane.native == nil:
			return base.RestampAdopted(ctx, dir, legEnv, beadID, fromAssignee, toAssignee)
		}
		return lane.native.TransferIfCurrent(beadID, fromAssignee, toAssignee)
	}
	ops.ReclaimStale = func(ctx context.Context, dir string, env []string, beadID string) (bool, string, error) {
		lane, scope, legEnv, remote, err := router.leg(dir, env)
		switch {
		case !remote:
			return base.ReclaimStale(ctx, dir, env, beadID)
		case err != nil:
			return false, "", err
		case lane.native == nil:
			return base.ReclaimStale(ctx, dir, legEnv, beadID)
		}
		// The native store has no stale-lease reclaim; the tier is skipped
		// for this leg, said once per invocation.
		if !router.warnedReclaim && router.stderr != nil {
			fmt.Fprintf(router.stderr, "gc hook --claim: skipping stale-lease reclaim for the remote work store at %s (the native store has no reclaim)\n", scope) //nolint:errcheck // best-effort stderr
			router.warnedReclaim = true
		}
		return false, "", nil
	}
	// Diagnostics that stay on the bd CLI on both lanes still reach the
	// remote server, so they carry the scope's credential too.
	ops.ConfirmBlocked = func(ctx context.Context, dir string, env []string, beadID, assignee string) (bool, error) {
		_, _, legEnv, remote, err := router.leg(dir, env)
		if !remote {
			return base.ConfirmBlocked(ctx, dir, env, beadID, assignee)
		}
		if err != nil {
			return false, err
		}
		return base.ConfirmBlocked(ctx, dir, legEnv, beadID, assignee)
	}
	ops.EmitExecutionStepStarted = func(step beads.Bead, dir string, env []string, assignee string) {
		_, _, legEnv, remote, err := router.leg(dir, env)
		if remote && err != nil {
			return
		}
		if !remote {
			legEnv = env
		}
		base.EmitExecutionStepStarted(step, dir, legEnv, assignee)
	}
	return ops
}
