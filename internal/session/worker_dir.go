package session

import "strings"

// WorkerDirFromInfo returns the agent process working directory recorded on a
// session, reading the canonical worker_dir mirror (Info.WorkerDir) first and
// falling back to the legacy work_dir mirror (Info.WorkDir) when the canonical
// value is absent or whitespace-only. Empty result means "no worker dir
// recorded."
//
// It is the session.Info form of contract.WorkerDirFromMetadata: because
// Info.WorkerDir mirrors beadmeta.WorkerDirMetadataKey verbatim and Info.WorkDir
// mirrors the legacy work_dir key verbatim, the canonical→legacy precedence and
// the whitespace-normalizing TrimSpace are byte-identical to the raw-metadata
// read. TestWorkerDirFromInfoMatchesContract pins that equivalence.
func WorkerDirFromInfo(info Info) string {
	if v := strings.TrimSpace(info.WorkerDir); v != "" {
		return v
	}
	return strings.TrimSpace(info.WorkDir)
}
