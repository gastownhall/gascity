//go:build linux

package proctable

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// These tests pin the ga-lp5w6 adjudication: an exact-incarnation sweep proves
// absence within the session's reachable scope, not across the whole host. A
// fleet host permanently carries owned-but-unreadable processes (sudo children
// and non-dumpable agents inside OTHER sessions' tmux pane scopes, sshd
// per-login processes under root's listener) that start after any given
// incarnation, so a completeness verdict over the entire owned process table
// is unreachable by construction — fail-closed had become fail-forever and
// every acked drain parked until its pool seat leaked.
//
// Two proofs narrow the verdict without weakening the protection:
//
//   - the live-pane-scope proof (licensed by the caller's own same-generation
//     tmux observation proving the target session absent): a process whose
//     lineage is anchored in a unique tmux pane spawn scope — its own, or the
//     nearest ancestor's when a fork escaped the scope (ga-f7v2ft.201) —
//     whose scope chain exits to a live, pre-incarnation spawner that does not
//     carry the target session ID belongs to some other pane;
//   - the foreign-lineage proof: a process whose parent chain is foreign-uid
//     all the way to a pre-incarnation ancestor (never touching init) has its
//     lineage rooted outside anything this session could have spawned.
//
// The controls keep every undecidable shape failing closed:
// TestScanWithRootSinceUnreadableSudoChildProofFailsClosed (unmodified) plus
// the in-scope/undecidable cases below.

const scanScopeTestSpawnerCgroup = "/gascity.slice/gascity-prod.slice/gascity-supervisor.service"

// buildForeignPaneScopeFixture builds the production stall shape: a
// pre-incarnation spawner (the tmux server) outside any pane scope, and a
// stranger pane whose root and sudo child are owned, non-dumpable and started
// AFTER the target incarnation. Returns the fixture root.
func buildForeignPaneScopeFixture(t *testing.T, spawnerEnv map[string]string, spawnerStartTicks uint64) string {
	t.Helper()
	root := t.TempDir()
	boot := time.Unix(1_700_000_000, 0).UTC()
	writeFakeBootTime(t, root, boot)

	const (
		spawnerPID  = 900
		paneRootPID = 901
		sudoKidPID  = 902
	)
	scope := "/user.slice/user-1000.slice/session-1.scope/tmux-spawn-feed1.scope"

	spawnerDir := filepath.Join(root, strconv.Itoa(spawnerPID))
	if err := os.MkdirAll(spawnerDir, 0o755); err != nil {
		t.Fatalf("create spawner fixture: %v", err)
	}
	writeFakeProcessUID(t, spawnerDir, os.Geteuid())
	writeFakeProcessStat(t, spawnerDir, spawnerPID, 1, spawnerStartTicks)
	writeFakeProcessCgroup(t, spawnerDir, scanScopeTestSpawnerCgroup)
	writeFakeProcessEnviron(t, spawnerDir, spawnerEnv)

	paneRootDir := filepath.Join(root, strconv.Itoa(paneRootPID))
	if err := os.MkdirAll(paneRootDir, 0o755); err != nil {
		t.Fatalf("create pane-root fixture: %v", err)
	}
	writeUnreadableEnviron(t, paneRootDir)
	writeFakeProcessUIDs(t, paneRootDir, os.Geteuid(), 0)
	writeFakeProcessStat(t, paneRootDir, paneRootPID, spawnerPID, 2000)
	writeFakeProcessCgroup(t, paneRootDir, scope)

	sudoKidDir := filepath.Join(root, strconv.Itoa(sudoKidPID))
	if err := os.MkdirAll(sudoKidDir, 0o755); err != nil {
		t.Fatalf("create sudo-child fixture: %v", err)
	}
	writeUnreadableEnviron(t, sudoKidDir)
	writeFakeProcessUIDs(t, sudoKidDir, os.Geteuid(), 0)
	writeFakeProcessStat(t, sudoKidDir, sudoKidPID, paneRootPID, 2100)
	writeFakeProcessCgroup(t, sudoKidDir, scope)

	return root
}

// buildEscapedSudoChildFixture builds the gci-f1289m specimen
// (ga-f7v2ft.201): the foreign-pane fixture with ONE fork left behind. The
// outer sudo migrated into the pane's spawn scope; its privileged child stayed
// in the cgroup the pane was spawned from — the supervisor's own service
// cgroup — so the candidate's OWN cgroup is not a tmux spawn scope even though
// its parent's is.
func buildEscapedSudoChildFixture(t *testing.T, spawnerEnv map[string]string, spawnerStartTicks uint64) string {
	t.Helper()
	root := buildForeignPaneScopeFixture(t, spawnerEnv, spawnerStartTicks)
	writeFakeProcessCgroup(t, filepath.Join(root, "902"), scanScopeTestSpawnerCgroup)
	return root
}

// TestScanWithRootSinceInScopeClearsEscapedSudoChildOfForeignPaneScope is the
// gci-f1289m production shape (ga-f7v2ft.201): a single privileged sudo child
// that escaped its pane's spawn scope denied absence for every incarnation
// younger than it, because both scope proofs keyed on the candidate's own
// cgroup and never reached the parent chain. The pane the fork belongs to is
// provably foreign, so the candidate inherits that exclusion and the sweep
// must read COMPLETE.
func TestScanWithRootSinceInScopeClearsEscapedSudoChildOfForeignPaneScope(t *testing.T) {
	root := buildEscapedSudoChildFixture(t, map[string]string{}, 500)
	boot := time.Unix(1_700_000_000, 0).UTC()

	got, err := scanWithRootSinceInScope(root, "ga-target", boot.Add(10*time.Second), SessionScope{
		TmuxSessionProvenAbsent: true,
	})
	if err != nil {
		t.Fatalf("escaped sudo child made the licensed exact scan incomplete: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("licensed exact scan returned %d runtimes, want 0", len(got))
	}
}

// TestScanWithRootSinceWithoutLicenseKeepsEscapedSudoChildClosed is the
// license control for the widened proof: without a same-generation proof that
// the target session is absent, the pane the fork belongs to could be the
// target's own.
func TestScanWithRootSinceWithoutLicenseKeepsEscapedSudoChildClosed(t *testing.T) {
	root := buildEscapedSudoChildFixture(t, map[string]string{}, 500)
	boot := time.Unix(1_700_000_000, 0).UTC()

	got, err := scanWithRootSince(root, "ga-target", boot.Add(10*time.Second))
	if err == nil {
		t.Fatalf("unlicensed scan = %v, nil error; want incomplete without the pane-absence proof", got)
	}
}

// TestScanWithRootSinceInScopeEscapedSudoChildProofFailsClosed keeps the
// inheritance honest: a candidate outside every spawn scope may borrow an
// ancestor's exclusion only when that ancestor's scope affirmatively proves
// OUT. An anchoring scope that merely exists — or a chain that never reaches
// one — leaves the candidate in the residue, which is what keeps the pinned
// sudo-child protection (TestScanWithRootSinceUnreadableSudoChildProofFailsClosed)
// meaningful.
func TestScanWithRootSinceInScopeEscapedSudoChildProofFailsClosed(t *testing.T) {
	boot := time.Unix(1_700_000_000, 0).UTC()
	incarnation := boot.Add(10 * time.Second)

	tests := []struct {
		name    string
		fixture func(t *testing.T) string
	}{
		{
			// The anchoring scope's spawner carries the target identity: that
			// pane could be the session's own, so nothing under it — inside the
			// scope or escaped from it — is excluded.
			name: "anchoring scope carries target identity",
			fixture: func(t *testing.T) string {
				return buildEscapedSudoChildFixture(t, map[string]string{"GC_SESSION_ID": "ga-target"}, 500)
			},
		},
		{
			// A post-incarnation spawner could itself belong to the target's
			// lineage, so its pane proves nothing about the escaped fork.
			name: "anchoring scope spawner postdates incarnation",
			fixture: func(t *testing.T) string {
				return buildEscapedSudoChildFixture(t, map[string]string{}, 1500)
			},
		},
		{
			// An unreadable spawner cannot say whose pane it spawned.
			name: "anchoring scope spawner environ unreadable",
			fixture: func(t *testing.T) string {
				root := buildEscapedSudoChildFixture(t, map[string]string{}, 500)
				if err := os.Remove(filepath.Join(root, "900", "environ")); err != nil {
					t.Fatalf("remove spawner environ: %v", err)
				}
				writeUnreadableEnviron(t, filepath.Join(root, "900"))
				return root
			},
		},
		{
			// Nothing in the chain is in a spawn scope: the whole lineage
			// escaped, so there is no pane to inherit an exclusion from.
			name: "chain reaches init without any spawn scope",
			fixture: func(t *testing.T) string {
				root := buildEscapedSudoChildFixture(t, map[string]string{}, 500)
				writeFakeProcessCgroup(t, filepath.Join(root, "901"), scanScopeTestSpawnerCgroup)
				return root
			},
		},
		{
			// The escaped fork re-parented to init: the link that named its
			// pane is gone, which is the orphaned sudo-child shape itself.
			name: "escaped fork re-parented to init",
			fixture: func(t *testing.T) string {
				root := buildEscapedSudoChildFixture(t, map[string]string{}, 500)
				writeFakeProcessStat(t, filepath.Join(root, "902"), 902, 1, 2100)
				return root
			},
		},
		{
			// The pane-scoped ancestor vanished between the walk and the
			// recheck: an unfinished proof never clears.
			name: "anchoring ancestor vanishes mid-proof",
			fixture: func(t *testing.T) string {
				root := buildEscapedSudoChildFixture(t, map[string]string{}, 500)
				if err := os.RemoveAll(filepath.Join(root, "901")); err != nil {
					t.Fatalf("remove pane-scoped ancestor: %v", err)
				}
				return root
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := tt.fixture(t)
			got, err := scanWithRootSinceInScope(root, "ga-target", incarnation, SessionScope{
				TmuxSessionProvenAbsent: true,
			})
			if err == nil {
				t.Fatalf("undecidable escaped-fork shape scan = %v, nil error; want incomplete", got)
			}
		})
	}
}

// TestScanWithRootSinceInScopeClearsStrangersInForeignLivePaneScopes is the
// production stall shape (ga-lp5w6): the target runtime is dead, and the only
// unreadable owned processes are a stranger pane root and its sudo child,
// both started after the incarnation, chained to a live pre-incarnation
// spawner that carries no session identity. With the target tmux session
// proven absent by the same observation generation, the sweep must read
// COMPLETE so the drain-ack stop can finalize.
func TestScanWithRootSinceInScopeClearsStrangersInForeignLivePaneScopes(t *testing.T) {
	root := buildForeignPaneScopeFixture(t, map[string]string{}, 500)
	boot := time.Unix(1_700_000_000, 0).UTC()

	got, err := scanWithRootSinceInScope(root, "ga-target", boot.Add(10*time.Second), SessionScope{
		TmuxSessionProvenAbsent: true,
	})
	if err != nil {
		t.Fatalf("foreign live-pane strangers made the licensed exact scan incomplete: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("licensed exact scan returned %d runtimes, want 0", len(got))
	}
}

// TestScanWithRootSinceWithoutLicenseKeepsPaneScopeStrangersClosed is the
// license control: the identical fixture WITHOUT the same-generation proof of
// target-session absence must keep failing closed — the pane could be the
// target's own.
func TestScanWithRootSinceWithoutLicenseKeepsPaneScopeStrangersClosed(t *testing.T) {
	root := buildForeignPaneScopeFixture(t, map[string]string{}, 500)
	boot := time.Unix(1_700_000_000, 0).UTC()

	got, err := scanWithRootSince(root, "ga-target", boot.Add(10*time.Second))
	if err == nil {
		t.Fatalf("unlicensed scan = %v, nil error; want incomplete without the pane-absence proof", got)
	}
}

// TestScanWithRootSinceInScopePaneProofFailsClosed keeps every
// undecidable-or-in-scope variant of the pane-scope shape incomplete, licensed
// or not: the proof must exclude only what it can prove excluded.
func TestScanWithRootSinceInScopePaneProofFailsClosed(t *testing.T) {
	boot := time.Unix(1_700_000_000, 0).UTC()
	incarnation := boot.Add(10 * time.Second)

	tests := []struct {
		name    string
		fixture func(t *testing.T) string
	}{
		{
			// The spawner readable and carrying the target identity: the pane
			// chain could be the session's own spawn lineage.
			name: "spawner carries target identity",
			fixture: func(t *testing.T) string {
				return buildForeignPaneScopeFixture(t, map[string]string{"GC_SESSION_ID": "ga-target"}, 500)
			},
		},
		{
			// A post-incarnation spawner could itself be part of the target's
			// lineage (an agent-started nested tmux server).
			name: "spawner postdates incarnation",
			fixture: func(t *testing.T) string {
				return buildForeignPaneScopeFixture(t, map[string]string{}, 1500)
			},
		},
		{
			// An unreadable spawner proves nothing about whose pane it spawned.
			name: "spawner environ unreadable",
			fixture: func(t *testing.T) string {
				root := buildForeignPaneScopeFixture(t, map[string]string{}, 500)
				spawnerEnviron := filepath.Join(root, "900", "environ")
				if err := os.Remove(spawnerEnviron); err != nil {
					t.Fatalf("remove spawner environ: %v", err)
				}
				writeUnreadableEnviron(t, filepath.Join(root, "900"))
				return root
			},
		},
		{
			// A pane root re-parented to init is the orphan shape the sudo-child
			// protection exists for: the chain no longer proves which pane the
			// scope belonged to.
			name: "pane chain exits to init",
			fixture: func(t *testing.T) string {
				root := buildForeignPaneScopeFixture(t, map[string]string{}, 500)
				writeFakeProcessStat(t, filepath.Join(root, "901"), 901, 1, 2000)
				return root
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := tt.fixture(t)
			got, err := scanWithRootSinceInScope(root, "ga-target", incarnation, SessionScope{
				TmuxSessionProvenAbsent: true,
			})
			if err == nil {
				t.Fatalf("undecidable pane shape scan = %v, nil error; want incomplete", got)
			}
		})
	}
}

// buildForeignLineageFixture builds the sshd shape: root's listener predates
// the incarnation, a per-connection foreign process postdates it, and the
// owned-but-unreadable session process hangs beneath both.
func buildForeignLineageFixture(t *testing.T, root string, listenerUID int) {
	t.Helper()
	const (
		listenerPID = 700
		connPID     = 701
		userPID     = 702
	)

	listenerDir := filepath.Join(root, strconv.Itoa(listenerPID))
	if err := os.MkdirAll(listenerDir, 0o755); err != nil {
		t.Fatalf("create listener fixture: %v", err)
	}
	writeFakeProcessUID(t, listenerDir, listenerUID)
	writeFakeProcessStat(t, listenerDir, listenerPID, 1, 100)

	connDir := filepath.Join(root, strconv.Itoa(connPID))
	if err := os.MkdirAll(connDir, 0o755); err != nil {
		t.Fatalf("create connection fixture: %v", err)
	}
	writeFakeProcessUID(t, connDir, listenerUID)
	writeFakeProcessStat(t, connDir, connPID, listenerPID, 2000)

	userDir := filepath.Join(root, strconv.Itoa(userPID))
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatalf("create user-process fixture: %v", err)
	}
	writeUnreadableEnviron(t, userDir)
	writeFakeProcessUID(t, userDir, os.Geteuid())
	writeFakeProcessStat(t, userDir, userPID, connPID, 2005)
	writeFakeProcessCgroup(t, userDir, "/user.slice/user-1000.slice/session-9.scope")
}

// TestScanWithRootSinceClearsUnreadableForeignLineageStrangers pins the sshd
// shape: an owned, unreadable per-login process started after the incarnation
// whose parent chain is foreign-uid up to a pre-incarnation ancestor. Nothing
// this session spawned can have its lineage rooted in a foreign process that
// predates the session, so the sweep stays complete. No license is required —
// the proof stands on the lineage alone.
func TestScanWithRootSinceClearsUnreadableForeignLineageStrangers(t *testing.T) {
	root := t.TempDir()
	boot := time.Unix(1_700_000_000, 0).UTC()
	writeFakeBootTime(t, root, boot)
	buildForeignLineageFixture(t, root, os.Geteuid()+1)

	got, err := scanWithRootSince(root, "ga-target", boot.Add(10*time.Second))
	if err != nil {
		t.Fatalf("foreign-lineage stranger made the exact scan incomplete: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("foreign-lineage scan returned %d runtimes, want 0", len(got))
	}
}

// TestScanWithRootSinceForeignLineageProofFailsClosed keeps the shapes the
// lineage proof cannot decide incomplete. The owned-ancestor control is the
// critical one: it is the exact chain a session's own sudo tree produces, and
// clearing it would delete the sudo-child protection.
func TestScanWithRootSinceForeignLineageProofFailsClosed(t *testing.T) {
	boot := time.Unix(1_700_000_000, 0).UTC()
	incarnation := boot.Add(10 * time.Second)

	tests := []struct {
		name    string
		fixture func(t *testing.T, root string)
	}{
		{
			// Our own uid on the pre-incarnation ancestor: this is a lineage the
			// session could own (agent → sudo → child), so it must fail closed.
			// The same fixture with a foreign ancestor completes (above), which
			// proves the uid check is what carries this control.
			name: "owned ancestor",
			fixture: func(t *testing.T, root string) {
				buildForeignLineageFixture(t, root, os.Geteuid())
			},
		},
		{
			// A candidate parented by init is the re-parented orphan shape —
			// exactly where a surviving session process lands. Never decidable.
			name: "candidate parented by init",
			fixture: func(t *testing.T, root string) {
				buildForeignLineageFixture(t, root, os.Geteuid()+1)
				writeFakeProcessStat(t, filepath.Join(root, "702"), 702, 1, 2005)
			},
		},
		{
			// A foreign chain that reaches init before any pre-incarnation
			// ancestor proves nothing about where the lineage is rooted.
			name: "foreign chain exits to init before predating",
			fixture: func(t *testing.T, root string) {
				buildForeignLineageFixture(t, root, os.Geteuid()+1)
				writeFakeProcessStat(t, filepath.Join(root, "701"), 701, 1, 2000)
			},
		},
		{
			// The predating ancestor vanishing mid-proof leaves the chain
			// unfinished; an unfinished proof never clears.
			name: "predating ancestor vanished",
			fixture: func(t *testing.T, root string) {
				buildForeignLineageFixture(t, root, os.Geteuid()+1)
				if err := os.RemoveAll(filepath.Join(root, "700")); err != nil {
					t.Fatalf("remove predating ancestor: %v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeFakeBootTime(t, root, boot)
			tt.fixture(t, root)

			got, err := scanWithRootSince(root, "ga-target", incarnation)
			if err == nil {
				t.Fatalf("undecidable lineage scan = %v, nil error; want incomplete", got)
			}
		})
	}
}

// TestScanWithRootSinceInScopeStillReturnsReadableMatch proves the widened
// proofs never touch positive evidence: a readable process carrying the target
// session ID is returned regardless of scope facts.
func TestScanWithRootSinceInScopeStillReturnsReadableMatch(t *testing.T) {
	root := buildForeignPaneScopeFixture(t, map[string]string{}, 500)
	boot := time.Unix(1_700_000_000, 0).UTC()
	buildFakeProc(t, root, 950, map[string]string{"GC_SESSION_ID": "ga-target"})
	writeFakeProcessStat(t, filepath.Join(root, "950"), 950, 1, 2200)

	got, err := scanWithRootSinceInScope(root, "ga-target", boot.Add(10*time.Second), SessionScope{
		TmuxSessionProvenAbsent: true,
	})
	if err != nil {
		t.Fatalf("licensed scan with a readable match failed: %v", err)
	}
	if len(got) != 1 || got[0].SessionID != "ga-target" || got[0].PID != 950 {
		t.Fatalf("licensed scan = %v, want the readable ga-target runtime pid 950", got)
	}
}
