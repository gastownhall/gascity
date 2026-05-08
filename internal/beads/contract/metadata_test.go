package contract

import "testing"

func TestWorkerDirFromMetadata_ReadsCanonicalKey(t *testing.T) {
	got := WorkerDirFromMetadata(map[string]string{
		WorkerDirKey: "/home/ds/gascity",
	})
	if got != "/home/ds/gascity" {
		t.Fatalf("WorkerDirFromMetadata = %q, want %q", got, "/home/ds/gascity")
	}
}

func TestWorkerDirFromMetadata_FallsBackToLegacyKey(t *testing.T) {
	// Existing session beads written before C1 only have work_dir.
	// Reads must keep working without rewriting the bead.
	got := WorkerDirFromMetadata(map[string]string{
		LegacyWorkDirKey: "/legacy/path",
	})
	if got != "/legacy/path" {
		t.Fatalf("WorkerDirFromMetadata = %q, want %q (legacy fallback)", got, "/legacy/path")
	}
}

func TestWorkerDirFromMetadata_CanonicalWinsOverLegacy(t *testing.T) {
	// During the rollout a bead may carry both keys (writer started
	// emitting worker_dir but the old work_dir was never cleared).
	// Canonical key takes precedence.
	got := WorkerDirFromMetadata(map[string]string{
		WorkerDirKey:     "/canonical",
		LegacyWorkDirKey: "/legacy",
	})
	if got != "/canonical" {
		t.Fatalf("WorkerDirFromMetadata = %q, want %q (canonical wins)", got, "/canonical")
	}
}

func TestWorkerDirFromMetadata_EmptyCanonicalFallsToLegacy(t *testing.T) {
	// Edge case: canonical key present but blank string. Treat as
	// "not set" and fall through to legacy.
	got := WorkerDirFromMetadata(map[string]string{
		WorkerDirKey:     "",
		LegacyWorkDirKey: "/legacy",
	})
	if got != "/legacy" {
		t.Fatalf("WorkerDirFromMetadata = %q, want %q (empty canonical → legacy)", got, "/legacy")
	}
}

func TestWorkerDirFromMetadata_WhitespaceNormalized(t *testing.T) {
	got := WorkerDirFromMetadata(map[string]string{
		WorkerDirKey: "  /trimmed  ",
	})
	if got != "/trimmed" {
		t.Fatalf("WorkerDirFromMetadata = %q, want %q (whitespace normalized)", got, "/trimmed")
	}
}

func TestWorkerDirFromMetadata_NilMap(t *testing.T) {
	if got := WorkerDirFromMetadata(nil); got != "" {
		t.Fatalf("WorkerDirFromMetadata(nil) = %q, want empty", got)
	}
}

func TestWorkerDirFromMetadata_AllEmpty(t *testing.T) {
	got := WorkerDirFromMetadata(map[string]string{
		WorkerDirKey:     "",
		LegacyWorkDirKey: "   ",
	})
	if got != "" {
		t.Fatalf("WorkerDirFromMetadata(all-empty) = %q, want empty", got)
	}
}

func TestArtifactDirFromMetadata_ReadsCanonicalKey(t *testing.T) {
	got := ArtifactDirFromMetadata(map[string]string{
		ArtifactDirKey: "/artifacts/bead-1",
	})
	if got != "/artifacts/bead-1" {
		t.Fatalf("ArtifactDirFromMetadata = %q, want %q", got, "/artifacts/bead-1")
	}
}

func TestArtifactDirFromMetadata_NoLegacyFallback(t *testing.T) {
	// ArtifactDir does NOT fall back to work_dir. Old task beads that
	// wrote work_dir under the conflated semantics were storing
	// artifact paths, but C1 explicitly does not migrate those —
	// GC_ARTIFACT_DIR projection from #1169 is the new source of truth.
	got := ArtifactDirFromMetadata(map[string]string{
		LegacyWorkDirKey: "/legacy/artifact",
	})
	if got != "" {
		t.Fatalf("ArtifactDirFromMetadata = %q, want empty (no legacy fallback)", got)
	}
}

func TestArtifactDirFromMetadata_NilMap(t *testing.T) {
	if got := ArtifactDirFromMetadata(nil); got != "" {
		t.Fatalf("ArtifactDirFromMetadata(nil) = %q, want empty", got)
	}
}

func TestSetWorkerDir_AllocatesNilMap(t *testing.T) {
	got := SetWorkerDir(nil, "/home/ds/gascity")
	if got == nil {
		t.Fatal("SetWorkerDir(nil) returned nil map; should allocate")
	}
	if got[WorkerDirKey] != "/home/ds/gascity" {
		t.Fatalf("SetWorkerDir result[%q] = %q, want %q", WorkerDirKey, got[WorkerDirKey], "/home/ds/gascity")
	}
}

func TestSetWorkerDir_PreservesOtherKeys(t *testing.T) {
	got := SetWorkerDir(map[string]string{
		"other":          "kept",
		LegacyWorkDirKey: "/legacy",
	}, "/canonical")
	if got["other"] != "kept" {
		t.Fatalf("SetWorkerDir clobbered other keys: %v", got)
	}
	if got[LegacyWorkDirKey] != "/legacy" {
		t.Fatalf("SetWorkerDir cleared legacy key: %v (caller's responsibility to clear if desired)", got)
	}
	if got[WorkerDirKey] != "/canonical" {
		t.Fatalf("SetWorkerDir result[%q] = %q, want %q", WorkerDirKey, got[WorkerDirKey], "/canonical")
	}
}

func TestSetArtifactDir_AllocatesNilMap(t *testing.T) {
	got := SetArtifactDir(nil, "/artifacts/bead-1")
	if got == nil {
		t.Fatal("SetArtifactDir(nil) returned nil map; should allocate")
	}
	if got[ArtifactDirKey] != "/artifacts/bead-1" {
		t.Fatalf("SetArtifactDir result[%q] = %q", ArtifactDirKey, got[ArtifactDirKey])
	}
}
