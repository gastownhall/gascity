package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

// rolledBackHandoffAdmissionVersion is the schema of the marker below. It is
// gc's own record, not a protocol artifact, so it may change with gc alone.
const rolledBackHandoffAdmissionVersion = 1

// rolledBackHandoffAdmissionFile is where gc records that it has taken a
// rolled-back scope back. It lives under .gc because it is gc's decision about
// bd's journal, not part of the journal: bd owns .beads/ownership-handoff.json
// and archives it on its own terms.
const rolledBackHandoffAdmissionFile = "beads-handoff-rollback-admitted.json"

type rolledBackHandoffAdmission struct {
	Version int    `json:"version"`
	Scope   string `json:"scope_path"`
	// Restored is a digest of the exact restoration this admission covers:
	// the journal's request identity plus the three workspace artifacts it
	// checkpointed, with their presence and modes. A later handoff generation
	// restores different bytes and gets its own admission.
	Restored   string `json:"restored_artifacts"`
	AdmittedAt string `json:"admitted_at"`
}

// admitRolledBackHandoff decides whether gc may resume managing a scope whose
// ownership handoff was compensated, and remembers the answer.
//
// The byte-exact comparison against the journal's checkpoint is the right rule
// for *deciding* that — bd putting metadata.json, config.yaml and the published
// port back exactly as it found them is the whole proof that the transfer was
// undone — but it is an admission gate for the rollback moment, not a standing
// invariant, and using it as one bricks the city.
//
// The rollback's own last step is bd asking gc to restart the legacy owner
// (`gc dolt-state start-managed`). That start publishes the managed runtime
// state, which reconciles the scope's canonical config, which merges gc's
// current bead vocabulary into types.custom — ordinary upgrade behavior for a
// city gc has just taken back, and a byte the journal's snapshot cannot
// contain because it predates the restart. Re-running the comparison on every
// later command therefore refused `gc start` and `gc stop` forever, on a city
// with a healthy legacy server and nothing wrong with it.
//
// So the gate answers once. A scope whose artifacts matched at first sight is
// recorded as admitted and afterwards follows the ordinary rules; a journal
// whose artifacts never matched is refused, and stays refused, because nothing
// has proven the rollback completed. bd's own archive rename of a journal that
// reaches rolled_back is the other way out, and needs nothing from gc: an
// archived journal is an absent journal, which is the legacy condition.
func admitRolledBackHandoff(scopeRoot string, journal handoffProjectionJournal) error {
	token, err := rolledBackHandoffRestoreToken(scopeRoot, journal)
	if err != nil {
		return err
	}
	admitted, err := rolledBackHandoffAlreadyAdmitted(scopeRoot, token)
	if err != nil {
		return err
	}
	if admitted {
		return nil
	}
	if err := handoffJournalRestoredArtifactsMatch(scopeRoot, restoredHandoffArtifacts(journal)); err != nil {
		return err
	}
	return recordRolledBackHandoffAdmission(scopeRoot, token)
}

// rolledBackHandoffRestoreToken names the restoration an admission covers.
//
// It digests the request identity and the checkpointed artifacts, which are the
// only part of the journal that does not move under the rollback: the target
// identity is retired, the evidence is re-recorded as the rollback advances,
// and the phase itself changes between legacy_config_restored and rolled_back.
// The bytes bd put back do not. Keying on them also makes a stale marker
// harmless — it can only re-admit a restoration of the very same bytes into the
// very same scope, which is the same decision gc already made.
func rolledBackHandoffRestoreToken(scopeRoot string, journal handoffProjectionJournal) (string, error) {
	body, err := json.Marshal(struct {
		Scope     string                               `json:"scope"`
		Request   any                                  `json:"request"`
		Artifacts map[string]handoffProjectionArtifact `json:"artifacts"`
	}{
		Scope: scopeRoot, Request: journal.Request, Artifacts: restoredHandoffArtifacts(journal),
	})
	if err != nil {
		return "", fmt.Errorf("digest restored ownership handoff artifacts: %w", err)
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func rolledBackHandoffAdmissionPath(scopeRoot string) string {
	return filepath.Join(scopeRoot, ".gc", rolledBackHandoffAdmissionFile)
}

// rolledBackHandoffAlreadyAdmitted reports whether this exact restoration has
// been admitted before. A marker gc cannot read is not an admission — the
// byte-exact gate runs again — but a marker it can read and that names another
// restoration is a refusal: something wrote a record gc did not.
func rolledBackHandoffAlreadyAdmitted(scopeRoot, token string) (bool, error) {
	path := rolledBackHandoffAdmissionPath(scopeRoot)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read rolled-back ownership handoff admission: %w", err)
	}
	var admission rolledBackHandoffAdmission
	if err := json.Unmarshal(data, &admission); err != nil {
		return false, nil
	}
	if admission.Version != rolledBackHandoffAdmissionVersion || !samePath(admission.Scope, scopeRoot) {
		return false, nil
	}
	return admission.Restored == token, nil
}

// recordRolledBackHandoffAdmission makes the admission durable before the
// caller's own lifecycle work can invalidate the gate it just passed.
//
// A write failure is reported rather than swallowed. The alternative — proceed
// unrecorded — succeeds once and then re-refuses on the next command with no
// trace of why, which is the failure this whole change exists to remove.
func recordRolledBackHandoffAdmission(scopeRoot, token string) error {
	path := rolledBackHandoffAdmissionPath(scopeRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("record rolled-back ownership handoff admission: %w", err)
	}
	body, err := json.MarshalIndent(rolledBackHandoffAdmission{
		Version:    rolledBackHandoffAdmissionVersion,
		Scope:      normalizePathForCompare(scopeRoot),
		Restored:   token,
		AdmittedAt: time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("record rolled-back ownership handoff admission: %w", err)
	}
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, path, append(body, '\n'), 0o644); err != nil {
		return fmt.Errorf("record rolled-back ownership handoff admission at %s: %w", path, err)
	}
	return nil
}
