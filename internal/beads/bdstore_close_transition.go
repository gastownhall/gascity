package beads

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"

	semver "github.com/Masterminds/semver/v3"
)

const (
	bdCloseTransitionMinVersion = "1.1.0"
)

type bdCloseIdentity struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	CloseReason     string `json:"close_reason"`
	ClosedBySession string `json:"closed_by_session"`
}

// CloseWithReasonIfOpen closes a live bead through bd's revision predicate.
// Gas City transition callers for the same scope also cooperate through the
// lifecycle lease, but exact ownership comes from --if-revision: a raw bd
// process that mutates the observed row first forces a precondition failure and
// can never be mistaken for this call merely because it used the same reason.
// bd still owns the mutation so close events, hooks, blocked-state
// recomputation, session attribution, and the Dolt commit path run.
func (s *BdStore) CloseWithReasonIfOpen(id, reason string) (CloseTransition, error) {
	if err := s.requireCloseTransitionSupport(); err != nil {
		return CloseTransition{}, err
	}
	lease, err := acquireLifecycleMutationLease(s.dir, inheritedLifecycleMutationFromEnv())
	if err != nil {
		return CloseTransition{}, fmt.Errorf("locking bd close transition for bead %q: %w", id, err)
	}
	defer lease.Unlock()

	before, err := s.readCanonicalTransitionSnapshot(id)
	if err != nil {
		return CloseTransition{}, err
	}
	preIdentity, err := s.readCloseIdentity(id)
	if err != nil {
		return CloseTransition{}, err
	}
	if mapBdStatus(preIdentity.Status) == "closed" {
		after, err := s.snapshotForCloseIdentity(id, preIdentity)
		if err != nil {
			return CloseTransition{}, err
		}
		return CloseTransition{Before: before, After: after}, nil
	}
	if before.Status == "closed" {
		return CloseTransition{}, fmt.Errorf("closing bead %q with reason: inconsistent pre-close status", id)
	}

	reason = closeReasonForTransition(before, reason)
	args := append(bdCloseTransitionArgs(id, reason), conditionalWriteFlag, strconv.FormatInt(before.Revision, 10))
	closeErr := s.runConditionalWriteWithEnv(id, before.Revision, lease.CommandEnv(), args...)

	postIdentity, identityErr := s.readCloseIdentity(id)
	if identityErr != nil {
		if closeErr != nil {
			return CloseTransition{}, errors.Join(fmt.Errorf("closing bead %q with reason: %w", id, closeErr), identityErr)
		}
		return CloseTransition{}, identityErr
	}
	if mapBdStatus(postIdentity.Status) != "closed" {
		if closeErr != nil {
			return CloseTransition{}, fmt.Errorf("closing bead %q with reason: %w", id, closeErr)
		}
		return CloseTransition{}, fmt.Errorf("closing bead %q with reason: bd close exited 0 but status is %q", id, postIdentity.Status)
	}
	after, err := s.snapshotForCloseIdentity(id, postIdentity)
	if err != nil {
		return CloseTransition{}, err
	}
	transition := CloseTransition{
		Before: before,
		After:  after,
	}
	if closeErr != nil {
		if errors.Is(closeErr, ErrConditionalWriteUnsupported) {
			return CloseTransition{}, fmt.Errorf("closing bead %q with reason: bd rejected revision fencing: %w", id, ErrCloseTransitionUnsupported)
		}
		if IsPreconditionFailed(closeErr) {
			// A closed post-state proves that another writer won from the
			// revision we observed. Preserve that winner as authoritative state,
			// but do not surface a routine lost close race as an operation error.
			return transition, nil
		}
		return transition, fmt.Errorf("closing bead %q with reason: %w", id, closeErr)
	}
	transition.Transitioned = true
	return transition, nil
}

func (s *BdStore) requireCloseTransitionSupport() error {
	s.closeTransitionMu.Lock()
	defer s.closeTransitionMu.Unlock()

	// Probe once per store. Operators must restart gc after changing bd
	// versions to re-evaluate atomic close support.
	if s.closeTransitionChecked {
		return s.closeTransitionErr
	}
	s.closeTransitionChecked = true

	out, err := s.runner(s.dir, "bd", "version")
	if err != nil {
		s.closeTransitionErr = fmt.Errorf(
			"bd close transition version gate: probing bd version: %w: %w",
			err,
			ErrCloseTransitionUnsupported,
		)
		return s.closeTransitionErr
	}
	version, err := parseBDVersion(string(out))
	if err != nil {
		s.closeTransitionErr = fmt.Errorf(
			"bd close transition version gate: parsing bd version: %w: %w",
			err,
			ErrCloseTransitionUnsupported,
		)
		return s.closeTransitionErr
	}
	parsedVersion, err := semver.StrictNewVersion(version)
	if err != nil {
		s.closeTransitionErr = fmt.Errorf(
			"bd close transition version gate: parsing bd version %q as strict semver: %w: %w",
			version,
			err,
			ErrCloseTransitionUnsupported,
		)
		return s.closeTransitionErr
	}
	if parsedVersion.Prerelease() != "" {
		s.closeTransitionErr = fmt.Errorf(
			"bd close transition requires a stable bd release (found %s): %w",
			version,
			ErrCloseTransitionUnsupported,
		)
		return s.closeTransitionErr
	}
	minimumVersion, err := semver.StrictNewVersion(bdCloseTransitionMinVersion)
	if err != nil {
		s.closeTransitionErr = fmt.Errorf(
			"bd close transition version gate: parsing minimum version %q: %w: %w",
			bdCloseTransitionMinVersion,
			err,
			ErrCloseTransitionUnsupported,
		)
		return s.closeTransitionErr
	}
	if parsedVersion.LessThan(minimumVersion) {
		s.closeTransitionErr = fmt.Errorf(
			"bd close transition requires bd %s or newer (found %s): %w",
			bdCloseTransitionMinVersion,
			version,
			ErrCloseTransitionUnsupported,
		)
		return s.closeTransitionErr
	}
	capable, capabilityErr := s.conditionalWritesCapable()
	if capabilityErr != nil {
		s.closeTransitionErr = fmt.Errorf(
			"bd close transition revision-fence probe failed: %w: %w",
			capabilityErr,
			ErrCloseTransitionUnsupported,
		)
	} else if !capable {
		s.closeTransitionErr = fmt.Errorf(
			"bd close transition requires bd %s support: %w",
			conditionalWriteFlag,
			ErrCloseTransitionUnsupported,
		)
	}
	return s.closeTransitionErr
}

func bdCloseTransitionArgs(id, reason string) []string {
	args := []string{"close", "--force", "--json"}
	if reason != "" {
		args = append(args, "--reason", reason)
	}
	return append(args, id)
}

func (s *BdStore) snapshotForCloseIdentity(id string, identity bdCloseIdentity) (Bead, error) {
	bead, err := s.readCanonicalTransitionSnapshot(id)
	if err != nil {
		return Bead{}, err
	}
	wantStatus := mapBdStatus(identity.Status)
	if bead.Status != wantStatus {
		return Bead{}, fmt.Errorf("reading close result for bead %q: status changed from %q to %q", id, wantStatus, bead.Status)
	}
	bead.Metadata = maps.Clone(bead.Metadata)
	if bead.Metadata == nil {
		bead.Metadata = make(StringMap)
	}
	if identity.CloseReason != "" {
		bead.Metadata["close_reason"] = identity.CloseReason
	}
	confirmed, err := s.readCloseIdentity(id)
	if err != nil {
		return Bead{}, err
	}
	if confirmed != identity {
		return Bead{}, fmt.Errorf("reading close result for bead %q: close identity changed while reading snapshot", id)
	}
	return bead, nil
}

// readCanonicalCloseSnapshot reads the exact logical bead through bd's search
// path. In bd 1.1, a wisp is canonical when a transient cross-table duplicate
// also leaves a stale issues row behind; bd show/GetIssue returns the issues
// row first, while mutations route to the wisp. Close transition snapshots must
// follow the mutation target or a successful wisp close can be reported as a
// status mismatch against the stale issues copy.
func (s *BdStore) readCanonicalCloseSnapshot(id string) (Bead, error) {
	if id == "" || !isBareBdQueryValue(id) {
		return Bead{}, fmt.Errorf("reading close snapshot for bead %q: invalid exact id", id)
	}
	out, err := s.runBDTransientRead("query", "--json", "id="+id, "--all", "--limit", "1")
	if err != nil {
		return Bead{}, fmt.Errorf("reading close snapshot for bead %q: %w", id, err)
	}
	issues, parseErr := parseIssuesTolerant(extractJSON(out))
	if parseErr != nil {
		return Bead{}, fmt.Errorf("reading close snapshot for bead %q: %w", id, parseErr)
	}
	for i := range issues {
		if issues[i].ID == id {
			return issues[i].toBead(), nil
		}
	}
	return Bead{}, fmt.Errorf("reading close snapshot for bead %q: %w", id, ErrNotFound)
}

// readCanonicalTransitionSnapshot completes bd query's canonical row with the
// dependency graph while the caller still owns the lifecycle lease. bd query
// 1.1 does not request dependency hydration, so publishing its row directly as
// a replacement event would erase edges from downstream projections.
func (s *BdStore) readCanonicalTransitionSnapshot(id string) (Bead, error) {
	bead, err := s.readCanonicalCloseSnapshot(id)
	if err != nil {
		return Bead{}, err
	}
	deps, err := s.DepList(id, "down")
	if err != nil {
		return Bead{}, fmt.Errorf("reading close snapshot dependencies for bead %q: %w", id, err)
	}
	bead.Dependencies = cloneDeps(deps)
	bead.Needs = nil
	return bead, nil
}

func (s *BdStore) readCloseIdentity(id string) (bdCloseIdentity, error) {
	query := bdCloseIdentitySQL(id)
	out, err := s.runBDTransientRead("sql", "--json", query)
	if err == nil {
		return parseBDCloseIdentity(out, id)
	}
	if isBdSQLUnsupportedInEmbeddedMode(err) {
		return s.readEmbeddedCloseIdentity(id, query)
	}
	if isBdCloseIdentityUnsupported(err) {
		return bdCloseIdentity{}, fmt.Errorf("reading close identity for bead %q: %w", id, ErrCloseTransitionUnsupported)
	}
	return bdCloseIdentity{}, fmt.Errorf("reading close identity for bead %q: %w", id, err)
}

func (s *BdStore) readEmbeddedCloseIdentity(id, query string) (bdCloseIdentity, error) {
	doltDir, ok, err := s.embeddedDoltDir()
	if err != nil {
		return bdCloseIdentity{}, fmt.Errorf("reading close identity for bead %q: %w", id, err)
	}
	if !ok {
		return bdCloseIdentity{}, fmt.Errorf("reading close identity for bead %q: %w", id, ErrCloseTransitionUnsupported)
	}
	out, err := s.runner(doltDir, "dolt", "sql", "-r", "json", "-q", query)
	if err != nil {
		return bdCloseIdentity{}, fmt.Errorf("reading close identity for bead %q: dolt sql: %w", id, err)
	}
	return parseBDCloseIdentity(out, id)
}

func bdCloseIdentitySQL(id string) string {
	literal := bdSQLStringLiteral(id)
	columns := "id,status,close_reason,closed_by_session"
	return "SELECT " + columns + " FROM wisps WHERE id = " + literal +
		" UNION ALL SELECT " + columns + " FROM issues WHERE id = " + literal +
		" AND NOT EXISTS (SELECT 1 FROM wisps WHERE id = " + literal + ") LIMIT 1"
}

func isBdCloseIdentityUnsupported(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unknown command \"sql\"") ||
		strings.Contains(message, "unknown subcommand \"sql\"") ||
		(strings.Contains(message, "bd sql") && strings.Contains(message, "not supported"))
}

func parseBDCloseIdentity(out []byte, id string) (bdCloseIdentity, error) {
	data := bytes.TrimSpace(extractJSON(out))
	var rows []bdCloseIdentity
	if err := json.Unmarshal(data, &rows); err == nil {
		if identity, ok := findBDCloseIdentity(rows, id); ok {
			return identity, nil
		}
	}

	type resultEnvelope struct {
		Rows []bdCloseIdentity `json:"rows"`
	}
	var envelope resultEnvelope
	if err := json.Unmarshal(data, &envelope); err == nil {
		if identity, ok := findBDCloseIdentity(envelope.Rows, id); ok {
			return identity, nil
		}
	}
	var envelopes []resultEnvelope
	if err := json.Unmarshal(data, &envelopes); err == nil {
		for _, result := range envelopes {
			if identity, ok := findBDCloseIdentity(result.Rows, id); ok {
				return identity, nil
			}
		}
	}
	return bdCloseIdentity{}, fmt.Errorf("reading close identity for bead %q: %w", id, ErrNotFound)
}

func findBDCloseIdentity(rows []bdCloseIdentity, id string) (bdCloseIdentity, bool) {
	for _, row := range rows {
		if row.ID == id {
			return row, true
		}
	}
	return bdCloseIdentity{}, false
}
