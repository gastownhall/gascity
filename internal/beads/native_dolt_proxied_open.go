package beads

import (
	"context"
	"fmt"
	"path/filepath"

	beadslib "github.com/steveyegge/beads"
)

// bdEnvPrefix is the namespace bd's own CLI configures itself from. It is not
// the library's namespace (that is beadsEnvPrefix) and gc's direct native opens
// have never had a reason to touch it — a direct scope is one gc configured,
// and bd is not in the loop.
//
// A proxied scope is the opposite: bd owns the proxy, its Dolt child and the
// migration decisions, and gc is opening the linked library against a database
// somebody else administers. Whatever BD_ variables the shell that launched gc
// happened to carry are that shell's opinion about bd's behavior, not gc's
// about the library's, and they must not ride into the window.
const bdEnvPrefix = "BD_"

// proxiedOnlyOpenEnvKeys are the keys the proxied-native window decides that a
// direct native open does not.
//
// They fall into two groups with opposite purposes.
//
// The first five are UNLOCKS, and they are listed so they are guaranteed
// UNSET: a key in an open-env key list whose value the projected map omits is
// actively unset for the window (see withProjectedOpenEnvLocked). bd reads
// BD_ALLOW_REMOTE_MIGRATE and BD_IGNORE_SCHEMA_SKEW to bypass exactly the gate
// gc runs before it admits a proxied database, BD_SMART_GATE and
// BD_EVENTS_JOURNAL change behavior gc has not accounted for, and
// BEADS_TEST_MODE relaxes the library. gc must not be able to migrate or
// schema-skew somebody else's shared database because the operator's shell
// exported an unlock for an unrelated bd command. The BEADS_/BD_ prefix
// withholding already removes all five, so naming them here is belt and braces
// rather than the load-bearing scrub — but it makes the list a complete written
// statement of what the window decides, which is what a reader auditing the
// hermetic claim needs.
//
// The last two are the AUTHOR PAIR, and they are projected with values. They
// are here because the projection reaches commit time, which is not obvious:
// beads' applyConfigDefaults reads GIT_AUTHOR_NAME and GIT_AUTHOR_EMAIL during
// the open, latches them onto the store instance, and commitAuthorString feeds
// that latched pair to DOLT_COMMIT(..., '--author', ?). The author is therefore
// pinned AT OPEN and not re-read at commit — so a window that failed to project
// them would attribute a gc write to whoever last ran a git command in that
// shell. PR2 serves no writes, but the pin is what PR3's authorship stands on.
var proxiedOnlyOpenEnvKeys = []string{
	"BD_ALLOW_REMOTE_MIGRATE",
	"BD_EVENTS_JOURNAL",
	"BD_IGNORE_SCHEMA_SKEW",
	"BD_SMART_GATE",
	"BEADS_TEST_MODE",
	"GIT_AUTHOR_EMAIL",
	"GIT_AUTHOR_NAME",
}

// proxiedNativeOpenEnvKeys is the open-env key list for the proxied-native
// window: everything a direct open decides, plus proxiedOnlyOpenEnvKeys.
//
// It is a SEPARATE list, and that separation is the whole point. Growing the
// shared nativeDoltOpenEnvKeys would change what a flag-off open does, because
// a listed key the caller's map omits is unset: adding GIT_AUTHOR_NAME there
// would strip an operator's git identity from every direct native open in the
// process, and adding BEADS_TEST_MODE would unset it under the library's own
// test suites. "Flag off is byte-identical" has to mean the direct projection
// is untouched, not merely that the new arm is unreachable.
var proxiedNativeOpenEnvKeys = append(append([]string(nil), nativeDoltOpenEnvKeys...), proxiedOnlyOpenEnvKeys...)

// OpenNativeDoltStoreAtProxied opens a native Dolt-backed store against a
// database served by bd's proxy, through a hermetic environment window.
//
// The window is one indivisible transition under nativeDoltOpenEnvMu:
//
//  1. withhold the whole BEADS_ and BD_ namespaces, so nothing the ambient
//     shell carries can re-point the open or unlock a migration;
//  2. project env over proxiedNativeOpenEnvKeys, which SETS the endpoint the
//     admission pin resolved and the author pair, and UNSETS every listed key
//     the caller did not name;
//  3. open, read the issue prefix while the env still stands;
//  4. restore both, in reverse.
//
// Holding nativeDoltOpenEnvMu across all four is what makes the process
// environment look atomic to ProcessEnvSnapshotExcludingNativeDoltOpen and to
// AmbientNativeDoltOpenEnv: a concurrent scope open or a bd-child env build
// blocks rather than reading this window's transient values.
//
// It does NOT decide whether the scope may be served natively. That is
// admission's job, which runs first and produces the pin this env is built
// from; this function is the mechanism, and it will faithfully open whatever it
// is pointed at.
func OpenNativeDoltStoreAtProxied(parent context.Context, scopeRoot string, env map[string]string, opts ...NativeDoltStoreOption) (*NativeDoltStore, error) {
	ctx, cancel := nativeDoltOperationContext(parent)
	defer cancel()
	storage, prefix, err := openNativeStorageProxied(ctx, scopeRoot, env, true)
	if err != nil {
		return nil, err
	}
	store := newNativeDoltStoreWithStorageAndPrefix(storage, nativeDoltStoreActor, prefix)
	store.localStrings = newLocalSidecar(filepath.Join(scopeRoot, ".beads", "local-strings.json"))
	// Structural, not optional: every handle this function produces is serving a
	// database bd's proxy owns, so its read path may name an endpoint fact as a
	// typed verdict. A caller cannot forget to ask for it, because a proxied
	// handle that classified its failures and then returned them untyped is
	// exactly the shape that leaves the wrapper sitting on a dead pool.
	store.proxiedReadVerdicts = true
	// And so is the read-only fence, for exactly the same reason (council
	// B-F5). It was left to WithProxiedReadOnly, which one call site passed —
	// so a SECOND proxied open site (PR3's write arm, a new rig path, an
	// acceptance seam) that omitted the option got a fully writable native
	// handle against a database bd owns, with no compile error, no runtime
	// signal, and a green read-only latch test (which builds its own latched
	// store). "No native write reaches a bd-owned database" is one claim with
	// two halves; leaving one half opt-in while the other is structural is how
	// they drift.
	//
	// PR3's delta is therefore an explicit WithProxiedWritable(), not the
	// ABSENCE of an option: a writable proxied handle should have to be asked
	// for by name, in a diff a reviewer reads.
	store.readOnlyReason = proxiedNativeReadOnlyReason
	for _, opt := range opts {
		opt(store)
	}
	return store, nil
}

// OpenNativeStorageAtProxied opens a bare storage handle through the same
// hermetic window, for a reopen hook: a reconnect after the proxy rebinds must
// re-project the CURRENT generation's endpoint, and it must do so under exactly
// the same withholding as the original open or the second handle would be
// configured by whatever the ambient environment says.
func OpenNativeStorageAtProxied(ctx context.Context, scopeRoot string, env map[string]string) (NativeStorage, error) {
	storage, _, err := openNativeStorageProxied(ctx, scopeRoot, env, false)
	return storage, err
}

func openNativeStorageProxied(ctx context.Context, scopeRoot string, env map[string]string, readPrefix bool) (beadslib.Storage, string, error) {
	nativeDoltOpenEnvMu.Lock()
	defer nativeDoltOpenEnvMu.Unlock()

	restoreNamespaces, err := withWithheldPrefixesLocked(beadsEnvPrefix, bdEnvPrefix)
	if err != nil {
		return nil, "", err
	}
	defer restoreNamespaces()

	restoreEnv, err := withProjectedOpenEnvLocked(proxiedNativeOpenEnvKeys, env, "")
	if err != nil {
		return nil, "", err
	}
	defer restoreEnv()

	storage, err := nativeDoltOpenBestAvailable(ctx, filepath.Join(scopeRoot, ".beads"))
	if err != nil {
		return nil, "", err
	}
	if !readPrefix {
		return storage, "", nil
	}
	prefix, err := storage.GetConfig(ctx, nativeIssuePrefixConfigKey)
	if err != nil {
		_ = storage.Close()
		return nil, "", fmt.Errorf("reading native issue prefix: %w", err)
	}
	return storage, prefix, nil
}
