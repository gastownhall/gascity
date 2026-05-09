package contract

import "strings"

// Canonical bead-metadata keys for the worker-path / artifact-path split
// (gastownhall/gascity#1251 Shift C, phase 1).
//
// The legacy `work_dir` key was overloaded: it meant "agent process cwd"
// when written on session beads and "work artifact directory" when
// written on task/molecule beads. resolveTaskWorkDir in
// cmd/gc/session_reconciler.go silently bridged the two semantics. The
// new keys split the meaning:
//
//	WorkerDirKey ("worker_dir")     — agent process cwd. Session beads.
//	ArtifactDirKey ("artifact_dir") — work artifact directory.
//	                                  Task / molecule beads.
//
// LegacyWorkDirKey is the old name. It MUST be kept readable on session
// beads via WorkerDirFromMetadata so existing data does not regress
// during the rollout. New writes should use WorkerDirKey.
const (
	// WorkerDirKey is the canonical session-bead metadata key for the
	// agent process working directory.
	WorkerDirKey = "worker_dir"

	// ArtifactDirKey is the canonical task/molecule-bead metadata key
	// for the work artifact directory. Distinct from WorkerDirKey to
	// stop conflating "where the agent runs" with "where artifacts
	// land" (#1101, originally #1094 — already fixed by #1169).
	ArtifactDirKey = "artifact_dir"

	// LegacyWorkDirKey is the deprecated metadata key that overloaded
	// the worker-cwd and artifact-dir semantics on different bead
	// types. Reads still fall back to it via WorkerDirFromMetadata so
	// existing session beads keep working during the rollout. New
	// writes should not use this key — a future CI lint will flag any
	// new writes outside the alias-shim path.
	LegacyWorkDirKey = "work_dir"
)

// WorkerDirFromMetadata returns the agent process working directory
// recorded on a session bead. Reads the canonical WorkerDirKey first
// and falls back to the legacy work_dir key when the canonical key is
// absent or empty. Empty result means "no worker dir recorded."
//
// Whitespace-only values are normalized to empty so callers do not
// have to TrimSpace at every read site.
func WorkerDirFromMetadata(meta map[string]string) string {
	if meta == nil {
		return ""
	}
	if v := strings.TrimSpace(meta[WorkerDirKey]); v != "" {
		return v
	}
	return strings.TrimSpace(meta[LegacyWorkDirKey])
}

// ArtifactDirFromMetadata returns the work artifact directory recorded
// on a task or molecule bead. NO fallback to legacy work_dir on this
// key path — task/molecule beads that wrote work_dir under the old
// semantics were storing artifact paths, but the C1 rollout treats the
// artifact-dir reading as new behavior driven by GC_ARTIFACT_DIR
// projection (#1169) rather than bead-stored convention. Migration of
// existing task-bead work_dir values is out of scope for this phase.
//
// Empty result means "no artifact dir recorded on this bead."
func ArtifactDirFromMetadata(meta map[string]string) string {
	if meta == nil {
		return ""
	}
	return strings.TrimSpace(meta[ArtifactDirKey])
}

// SetWorkerDir writes the canonical WorkerDirKey on a metadata map,
// creating the map if nil. Returns the (possibly newly allocated) map
// so call sites can use a fluent style:
//
//	meta = contract.SetWorkerDir(meta, "/home/ds/gascity")
//
// Empty values are passed through (allowing callers to clear the key
// by passing ""). Does not touch LegacyWorkDirKey — coexistence with
// the deprecated key is the reader's responsibility via
// WorkerDirFromMetadata.
func SetWorkerDir(meta map[string]string, dir string) map[string]string {
	if meta == nil {
		meta = make(map[string]string, 1)
	}
	meta[WorkerDirKey] = dir
	return meta
}

// SetArtifactDir writes the canonical ArtifactDirKey on a metadata
// map, creating the map if nil. Mirrors SetWorkerDir.
func SetArtifactDir(meta map[string]string, dir string) map[string]string {
	if meta == nil {
		meta = make(map[string]string, 1)
	}
	meta[ArtifactDirKey] = dir
	return meta
}
