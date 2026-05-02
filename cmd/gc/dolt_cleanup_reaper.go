package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

// DoltProcInfo describes a live `dolt sql-server` process candidate.
//
// PID is the OS pid; Argv is the raw command line split on NUL boundaries
// (typically read from /proc/<pid>/cmdline). Ports lists the TCP ports the
// process is listening on, used to cross-reference against active per-rig
// dolt servers so the reaper never touches a production server. RSSBytes is
// the best-effort resident set size used for operator cleanup summaries.
// StartTimeTicks is /proc/<pid>/stat field 22 and lets force-mode revalidation
// detect PID reuse before sending a signal.
type DoltProcInfo struct {
	PID            int
	Argv           []string
	Ports          []int
	RSSBytes       int64
	StartTimeTicks uint64
}

// reapClassification is the per-process decision produced by classifyDoltProcess.
//
// Action is "reap" or "protect". For reap, ConfigPath carries the test-config
// path that matched the allowlist. For protect, Reason explains why so the
// operator-facing report can echo it (e.g. "active rig dolt server (rig: beads)").
type reapClassification struct {
	Action     string
	Reason     string
	ConfigPath string
}

// ReapTarget is a single PID slated for SIGTERM+SIGKILL during the reap stage.
type ReapTarget struct {
	PID            int
	ConfigPath     string
	RSSBytes       int64
	StartTimeTicks uint64
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

// isTestConfigPath reports whether p matches the architect-specified test
// allowlist (§4.3 step 3): /tmp/Test*, <tempDir>/Test*, or
// <homeDir>/.gotmp/Test*. The leading `Test` prefix matches Go's
// testing-package convention; `go test` writes tmp dirs under those roots when
// fixtures spin up dolt sql-server.
func isTestConfigPath(p, homeDir, tempDir string) bool {
	if p == "" {
		return false
	}
	clean := filepath.Clean(p)
	if hasTestChildPrefix(clean, "/tmp") {
		return true
	}
	if hasTestChildPrefix(clean, tempDir) {
		return true
	}
	if homeDir == "" {
		return false
	}
	return hasTestChildPrefix(clean, filepath.Join(homeDir, ".gotmp"))
}

func hasTestChildPrefix(cleanPath, root string) bool {
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
	return strings.HasPrefix(strings.TrimPrefix(cleanPath, rootPrefix), "Test")
}

// classifyDoltProcess applies the architect's reaper decision rules (§4.3) to a
// single dolt sql-server process. Order matters:
//
//  1. Any port match against rigPortByPort → protected (active rig server),
//     even if the cmdline says it's a test path (defense in depth).
//  2. Else extract --config path; matches /tmp/Test*, os.TempDir()/Test*,
//     or ~/.gotmp/Test* → reap.
//  3. Else protect with a reason that echoes the actual config path so
//     operators can decide whether to kill it manually (architect Open Q 0).
func classifyDoltProcess(p DoltProcInfo, rigPortByPort map[int]string, homeDir, tempDir string) reapClassification {
	for _, port := range p.Ports {
		if name, ok := rigPortByPort[port]; ok {
			return reapClassification{
				Action: "protect",
				Reason: fmt.Sprintf("active rig dolt server (rig: %s, port: %d)", name, port),
			}
		}
	}

	cfgPath := extractConfigPath(p.Argv)
	if cfgPath == "" {
		return reapClassification{
			Action: "protect",
			Reason: "no --config path detected; refusing to kill an unidentified dolt server",
		}
	}
	if isTestConfigPath(cfgPath, homeDir, tempDir) {
		return reapClassification{Action: "reap", ConfigPath: cfgPath}
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
func planOrphanReap(procs []DoltProcInfo, rigPortByPort map[int]string, homeDir, tempDir string) ReapPlan {
	plan := ReapPlan{}
	for _, p := range procs {
		c := classifyDoltProcess(p, rigPortByPort, homeDir, tempDir)
		switch c.Action {
		case "reap":
			plan.Reap = append(plan.Reap, ReapTarget{
				PID:            p.PID,
				ConfigPath:     c.ConfigPath,
				RSSBytes:       p.RSSBytes,
				StartTimeTicks: p.StartTimeTicks,
			})
		default:
			plan.Protected = append(plan.Protected, ProtectedProcess{PID: p.PID, Reason: c.Reason})
		}
	}
	return plan
}
