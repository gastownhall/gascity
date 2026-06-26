package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Observed filesystem state for a per-process path (cwd, --config). The zero
// value is "unknown": discovery could not determine the state (no /proc on
// this host, readlink failed, relative path). Classification treats unknown
// as no signal, so it always degrades toward protection.
const (
	procPathStateUnknown = ""
	procPathStateLive    = "live"
	procPathStateDeleted = "deleted"
)

// DoltProcInfo describes a live `dolt sql-server` process candidate.
//
// PID is the OS pid; Argv is the raw command line split on NUL boundaries
// (typically read from /proc/<pid>/cmdline). Ports lists the TCP ports the
// process is listening on, used to cross-reference against active per-rig
// dolt servers so the reaper never touches a production server. RSSBytes is
// the best-effort resident set size used for operator cleanup summaries.
// StartTimeTicks is /proc/<pid>/stat field 22 and lets force-mode revalidation
// detect PID reuse before sending a signal. StartIdentity is a portable
// fallback populated by ps-based discovery on hosts without /proc.
//
// CWDState is procPathStateDeleted only on definitive evidence that
// /proc/<pid>/cwd is an unlinked inode — the kernel marks such a target with a
// trailing " (deleted)", which can never revert (renames show the new path
// instead) and is confirmed against the literal path by inode identity.
// procPathStateLive means the cwd resolves cleanly; procPathStateUnknown
// covers a host with no /proc, a failed readlink, or a non-definitive stat
// error or timeout during disambiguation, so ambiguous evidence always
// degrades toward protection. ConfigPathState records the same tri-state for
// the absolute --config path from Argv: deleted when the file no longer exists
// on disk, live when it does, unknown for absent or relative configs and for
// stat errors. ConfigPathState is not a standalone reap trigger: a deleted
// config protects (with a confirm-manually reason) unless the deleted-cwd
// signal corroborates that the scope is truly gone.
type DoltProcInfo struct {
	PID             int
	Argv            []string
	Ports           []int
	RSSBytes        int64
	StartTimeTicks  uint64
	StartIdentity   string
	CWDState        string
	ConfigPathState string
}

// reapClassification is the per-process decision produced by classifyDoltProcess.
//
// Action is "reap" or "protect". For reap, ConfigPath carries the --config
// path observed on the cmdline (empty for bare servers). Reason explains the
// decision so the operator-facing report can echo it: always set for protect
// (e.g. "active rig dolt server (rig: beads)") and set for deleted-scope
// reaps (deleted cwd, vanished config); empty for the classic
// test-config-path allowlist reap where the path itself is the explanation.
type reapClassification struct {
	Action     string
	Reason     string
	ConfigPath string
}

// ReapTarget is a single PID slated for SIGTERM+SIGKILL during the reap stage.
// Reason mirrors reapClassification.Reason for deleted-scope targets.
type ReapTarget struct {
	PID            int
	ConfigPath     string
	Reason         string
	RSSBytes       int64
	StartTimeTicks uint64
	StartIdentity  string
}

// ProtectedProcess is a single PID that the reaper refused to kill, with the
// reason recorded so the report can show operators why nothing was done.
type ProtectedProcess struct {
	PID    int
	Reason string
}

// ReapPlan is the outcome of planOrphanReap. Reap is the orphan list; Protected
// covers production-side rigs and unknown processes that fall outside the
// test-config-path allowlist (e.g. an active benchmark).
type ReapPlan struct {
	Reap      []ReapTarget
	Protected []ProtectedProcess
}

// extractConfigPath pulls the --config <path> argument from a dolt sql-server
// argv. Supports both `--config foo` and `--config=foo` forms; returns empty
// when the flag is absent or has no value.
func extractConfigPath(argv []string) string {
	for i, arg := range argv {
		if arg == "--config" {
			if i+1 < len(argv) {
				return argv[i+1]
			}
			return ""
		}
		if strings.HasPrefix(arg, "--config=") {
			return strings.TrimPrefix(arg, "--config=")
		}
	}
	return ""
}

// isTestConfigPath reports whether p matches the cleanup allowlist for test
// Dolt configs: Go test temp roots, plus known Gas City unit-test prefixes
// that use short socket-safe directories under os.TempDir().
func isTestConfigPath(p, homeDir, tempDir string) bool {
	if p == "" {
		return false
	}
	clean := filepath.Clean(p)
	if hasTestChildPrefix(clean, "/tmp", testConfigPathPrefixes()) {
		return true
	}
	if hasTestChildPrefix(clean, tempDir, testConfigPathPrefixes()) {
		return true
	}
	if homeDir == "" {
		return false
	}
	return hasTestChildPrefix(clean, filepath.Join(homeDir, ".gotmp"), []string{"Test"})
}

func testConfigPathPrefixes() []string {
	return []string{
		"Test",
		// Legacy pre-owner-PID cmd/gc test roots. Current cmd/gc roots use
		// the gct<PID>-* prefix and are handled by stale-root owner PID logic.
		"gctest-",
		"gc-state-runtime-builtin-",
		"gc-state-mutation-builtin-",
		"gc-supervisor-city-",
		"gc-reload-invalid-",
		"gc-rename-",
		"gcit-",
		"gc-int-env-",
	}
}

func hasTestChildPrefix(cleanPath, root string, prefixes []string) bool {
	if root == "" {
		return false
	}
	cleanRoot := filepath.Clean(root)
	if cleanRoot == "." || cleanRoot == string(filepath.Separator) {
		return false
	}
	rootPrefix := cleanRoot + string(filepath.Separator)
	if !strings.HasPrefix(cleanPath, rootPrefix) {
		return false
	}
	child := strings.TrimPrefix(cleanPath, rootPrefix)
	for _, prefix := range prefixes {
		if strings.HasPrefix(child, prefix) {
			return true
		}
	}
	return false
}

func configUnderActiveTestRoot(configPath string, activeTestRoots []string) bool {
	if configPath == "" {
		return false
	}
	cleanConfig := filepath.Clean(configPath)
	for _, root := range activeTestRoots {
		cleanRoot := filepath.Clean(root)
		if cleanRoot == "." || cleanRoot == string(filepath.Separator) {
			continue
		}
		if cleanConfig == cleanRoot || strings.HasPrefix(cleanConfig, cleanRoot+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// classifyDoltProcess applies the architect's reaper decision rules (§4.3) to a
// single dolt sql-server process. Order matters:
//
//  1. Any port match against rigPortByPort → protected (active rig server),
//     even if the cmdline says it's a test path or its scope looks deleted
//     (defense in depth).
//  2. Else protect if the --config sits under an active test root, even when
//     the config file itself is momentarily gone (mid-teardown of a test
//     that is still running).
//  3. Else reap when the working directory is an unlinked inode (ga-10wmzh):
//     a cwd readlink ending in " (deleted)" can never revert, so it proves the
//     scope is gone — this also covers bare servers started without --config.
//  4. Else protect bare servers (no --config): an unidentified dolt server is
//     never killed.
//  5. Else reap when --config is on the test-config-path allowlist (/tmp/Test*,
//     os.TempDir()/Test*, known Gas City temp prefixes). The allowlist match
//     is an ownership signal, so an owned test scope is reaped even if its
//     --config file was already removed.
//  6. Else, if a non-allowlist --config has vanished while the cwd is still
//     live or its state is unknown, protect with a confirm-and-kill-manually
//     reason: a lone missing-config observation is not proof of scope deletion,
//     so it reaps only with cross-signal corroboration (a confirmed deleted
//     cwd, checked in step 3) or an ownership signal (the allowlist in step 5).
//     Otherwise protect with a reason that echoes the actual config path so
//     operators can decide whether to kill it manually (architect Open Q 0).
//     Unknown state is never a reap signal.
func classifyDoltProcess(p DoltProcInfo, rigPortByPort map[int]string, homeDir, tempDir string, activeTestRoots []string) reapClassification {
	for _, port := range p.Ports {
		if name, ok := rigPortByPort[port]; ok {
			return reapClassification{
				Action: "protect",
				Reason: fmt.Sprintf("active rig dolt server (rig: %s, port: %d)", name, port),
			}
		}
	}

	cfgPath := extractConfigPath(p.Argv)
	if configUnderActiveTestRoot(cfgPath, activeTestRoots) {
		return reapClassification{
			Action:     "protect",
			Reason:     fmt.Sprintf("config %q is under an active test root", cfgPath),
			ConfigPath: cfgPath,
		}
	}
	if p.CWDState == procPathStateDeleted {
		return reapClassification{
			Action:     "reap",
			Reason:     "working directory deleted (scope removed)",
			ConfigPath: cfgPath,
		}
	}
	if cfgPath == "" {
		return reapClassification{
			Action: "protect",
			Reason: "no --config path detected; refusing to kill an unidentified dolt server",
		}
	}
	if isTestConfigPath(cfgPath, homeDir, tempDir) {
		// A test-config-path match is itself an ownership signal, so an owned
		// test scope is reaped even when its --config file was already removed.
		return reapClassification{Action: "reap", ConfigPath: cfgPath}
	}
	if p.ConfigPathState == procPathStateDeleted {
		// A non-allowlist --config has vanished while the working directory
		// checked above is not a confirmed unlinked inode (it is live, or its
		// state could not be determined). A lone missing-config observation is
		// not proof the owning scope was removed: a config can be momentarily
		// absent during a crash-adoption window or a transient rename. This
		// reaper therefore never acts on the missing-config signal by itself —
		// it requires cross-signal corroboration (a confirmed deleted cwd,
		// checked above) or an ownership signal (the test-config-path
		// allowlist). That is a different mechanism from the scope-death
		// watchdog (dolt_scope_watchdog.go), which instead waits for repeated
		// temporal confirmation of the same anchor before terminating a
		// supervised server. Without corroboration, protect and report rather
		// than risk killing a healthy or non-owned server.
		cwdDesc := "is still live"
		if p.CWDState != procPathStateLive {
			// CWDState is unknown here (deleted was reaped above): the ps
			// fallback or a readlink/stat failure left it undetermined, so the
			// reason must not claim the cwd was confirmed live.
			cwdDesc = "could not be determined"
		}
		return reapClassification{
			Action:     "protect",
			Reason:     fmt.Sprintf("config %q is missing but the working directory %s; not reaping on the missing-config signal alone — confirm the scope is gone and kill manually if unwanted", cfgPath, cwdDesc),
			ConfigPath: cfgPath,
		}
	}
	return reapClassification{
		Action: "protect",
		Reason: fmt.Sprintf("config %q not on test-config-path allowlist; kill manually if not wanted", cfgPath),
		// ConfigPath echoed so the human-readable layout (Wireframe 4) can
		// render the tree-style annotation alongside the port and reason.
		ConfigPath: cfgPath,
	}
}

// planOrphanReap classifies each dolt sql-server process and partitions them
// into reap targets vs protected processes. Order is preserved so the report
// renders deterministically.
func planOrphanReap(procs []DoltProcInfo, rigPortByPort map[int]string, homeDir, tempDir string, activeTestRoots []string) ReapPlan {
	plan := ReapPlan{}
	for _, p := range procs {
		c := classifyDoltProcess(p, rigPortByPort, homeDir, tempDir, activeTestRoots)
		switch c.Action {
		case "reap":
			plan.Reap = append(plan.Reap, ReapTarget{
				PID:            p.PID,
				ConfigPath:     c.ConfigPath,
				Reason:         c.Reason,
				RSSBytes:       p.RSSBytes,
				StartTimeTicks: p.StartTimeTicks,
				StartIdentity:  p.StartIdentity,
			})
		default:
			plan.Protected = append(plan.Protected, ProtectedProcess{PID: p.PID, Reason: c.Reason})
		}
	}
	return plan
}
