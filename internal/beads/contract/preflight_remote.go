package contract

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	beadsbackend "github.com/steveyegge/beads/backend"
)

// The remote route of the native-store preflight (DESIGN C2/C4).
//
// A scope whose .beads/metadata.json names a backend the linked beads library
// registered as REMOTE is served natively over the wire. None of the Dolt
// checks apply to it: there is no bd context to cross-check, no Dolt server to
// dial for a project id, no schema cursor to read. What does apply is the
// server itself, and wire_compat checks it over the handshake.

// RemoteWireRevisionMin and RemoteWireRevisionMax bound the server
// wire_revision the linked beads http client speaks: its compiled
// ClientMinWireRevision and ClientWireRevision (beads
// internal/httpclient/wire/handshake.go). beads exports neither, so they
// cannot be referenced from here; instead they are PINNED to the linked
// client by behavior: internal/beads
// TestRemoteWireRevisionRangeMatchesTheLinkedClient drives the real
// bdhttp.Handshake against a server reporting RemoteWireRevisionMax (which
// the client must accept) and RemoteWireRevisionMax+1 (which it must refuse
// with its own skew error), so a beads pin bump that moves the client's
// revision fails that test until these move with it. An absent wire_revision
// decodes as 0 and is accepted, exactly as the bd client accepts it.
const (
	RemoteWireRevisionMin = 0
	RemoteWireRevisionMax = 2
)

// RemoteCapabilityClass says what an absent capability token costs the native
// store.
type RemoteCapabilityClass string

const (
	// RemoteCapabilityRequired is a token the native store cannot work without:
	// an absent one FAILs wire_compat.
	RemoteCapabilityRequired RemoteCapabilityClass = "required"
	// RemoteCapabilityOptional is a token with a fallback the store already
	// takes: an absent one WARNs and records the fallback.
	RemoteCapabilityOptional RemoteCapabilityClass = "optional"
)

// RemoteCapabilityRequirement is one row of the class requirement table.
type RemoteCapabilityRequirement struct {
	Token    string
	Class    RemoteCapabilityClass
	Fallback string
}

// remoteCapabilityRequirements is the class requirement table, as data. A row
// moves between classes, or joins the table, by editing this list; the check
// below has no per-token logic.
var remoteCapabilityRequirements = []RemoteCapabilityRequirement{
	{Token: "issues.get", Class: RemoteCapabilityRequired},
	{Token: "issues.list", Class: RemoteCapabilityRequired},
	{Token: "issues.query", Class: RemoteCapabilityRequired},
	{Token: "issues.count", Class: RemoteCapabilityRequired},
	{Token: "issues.create", Class: RemoteCapabilityRequired},
	{Token: "issues.update", Class: RemoteCapabilityRequired},
	{Token: "issues.close", Class: RemoteCapabilityRequired},
	{Token: "issues.reopen", Class: RemoteCapabilityRequired},
	{Token: "issues.delete", Class: RemoteCapabilityRequired},
	{Token: "issues.claim", Class: RemoteCapabilityRequired},
	{Token: "issues.release", Class: RemoteCapabilityRequired},
	{Token: "issues.casMetadata", Class: RemoteCapabilityRequired},
	{Token: "issues.batchApply", Class: RemoteCapabilityRequired},
	{Token: "issues.batchClose", Class: RemoteCapabilityRequired},
	{Token: "ready.list", Class: RemoteCapabilityRequired},
	{Token: "ready.count", Class: RemoteCapabilityRequired},
	{Token: "dependencies.add", Class: RemoteCapabilityRequired},
	{Token: "dependencies.remove", Class: RemoteCapabilityRequired},
	{Token: "dependencies.list", Class: RemoteCapabilityRequired},
	{Token: "config.get", Class: RemoteCapabilityRequired},
	// issues.related and stats.get are REQUIRED, not optional: gc has no
	// fallback for either. issues.related answers DepList's dependents leg
	// (dependentDeps), and stats.get is the native store's Ping
	// (pingUpstreamRead), which the rig-accessibility and scope-readiness
	// waits poll. Without them those calls fail with the client's
	// capability refusal rather than degrading.
	{Token: "issues.related", Class: RemoteCapabilityRequired},
	{Token: "stats.get", Class: RemoteCapabilityRequired},
	{Token: "issues.batchGet", Class: RemoteCapabilityOptional, Fallback: "per-id issues.get"},
	{Token: "issues.count.scope", Class: RemoteCapabilityOptional, Fallback: "hydrating list count"},
	{Token: "issues.batchApplyLarge", Class: RemoteCapabilityOptional, Fallback: "graph plans capped at the batch limit"},
	{Token: "issues.reclaim", Class: RemoteCapabilityOptional, Fallback: "no lease reclaim"},
}

// RemoteCapabilityRequirements returns a copy of the class requirement table.
func RemoteCapabilityRequirements() []RemoteCapabilityRequirement {
	return slices.Clone(remoteCapabilityRequirements)
}

// PreflightWireSnapshot is what a remote server said about itself at
// handshake. It mirrors the beads library's curated snapshot so this package
// stays free of the transport.
type PreflightWireSnapshot struct {
	APIVersion   string
	BdVersion    string
	WireRevision int
	ProjectID    string
	Capabilities []string
}

// PreflightWireHandshake is one handshake read: the server's snapshot plus the
// project the scope's activation pins, when it pins one.
type PreflightWireHandshake struct {
	Snapshot PreflightWireSnapshot
	// TargetProjectID is the project the scope's remote activation pins
	// (beads' http_target.json expect_project_id). It is the remote backend's
	// identity of record; metadata.json's project_id is consulted only when it
	// is empty.
	TargetProjectID string
}

// PreflightWireReason classifies a failed handshake for diagnostics.
type PreflightWireReason string

// Typed handshake failure reasons.
const (
	PreflightWireProjectMismatch PreflightWireReason = "project_mismatch"
	PreflightWireAPIVersion      PreflightWireReason = "api_version"
	PreflightWireCapability      PreflightWireReason = "capability"
	PreflightWireUnauthenticated PreflightWireReason = "unauthenticated"
	PreflightWireNotConnected    PreflightWireReason = "not_connected"
	PreflightWireUnreachable     PreflightWireReason = "unreachable"
)

// PreflightWireError is a handshake failure with its typed reason.
type PreflightWireError struct {
	Reason PreflightWireReason
	Err    error
}

// Error implements error.
func (e *PreflightWireError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		return string(e.Reason)
	}
	return fmt.Sprintf("%s: %v", e.Reason, e.Err)
}

// Unwrap returns the underlying handshake error.
func (e *PreflightWireError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// BackendIsRemote reports whether backend names a remote backend registered
// with the linked beads library. The registry is populated at gc's
// composition root (internal/beads.RegisterRemoteBackends); a build that never
// registers one has no remote backend, and such a scope takes the ordinary
// unsupported-backend route.
func BackendIsRemote(backend string) bool {
	name := strings.TrimSpace(backend)
	if name == "" || NativeStoreServesBackend(name) {
		return false
	}
	return beadsbackend.IsRemote(name)
}

func (c PreflightChecker) backendIsRemote(backend string) bool {
	if strings.TrimSpace(backend) == "" {
		return false
	}
	if c.RemoteBackend != nil {
		return c.RemoteBackend(backend)
	}
	return BackendIsRemote(backend)
}

// remoteNotConsultedChecks reports the Dolt-route checks for a remote scope.
// They PASS with a "not consulted" summary: nothing about a Dolt server can be
// true or false of a scope served over the wire, and a WARN here would degrade
// every remote scope for a probe that does not apply to it.
func remoteNotConsultedChecks(backend string) []PreflightCheckResult {
	details := PreflightDetails{MetadataBackend: backend}
	ids := []PreflightCheckID{
		PreflightCheckBDContextAgreement,
		PreflightCheckDoltModeSafe,
		PreflightCheckIdentityMatch,
		PreflightCheckVersionCompat,
		PreflightCheckBDVersionHint,
		PreflightCheckContractShape,
	}
	checks := make([]PreflightCheckResult, 0, len(ids))
	for _, id := range ids {
		summary := "not consulted (remote backend)"
		if id == PreflightCheckIdentityMatch {
			summary = "not consulted (remote backend); identity is checked by wire_compat"
		}
		checks = append(checks, NewPreflightCheckResult(id, PreflightCheckPass, summary, details))
	}
	return checks
}

// checkRemote is Check's remote route.
func (c PreflightChecker) checkRemote(scope string, metadata preflightMetadata, providerCheck PreflightCheckResult) PreflightResult {
	metadataCheck := NewPreflightCheckResult(PreflightCheckMetadataBackend, PreflightCheckPass,
		fmt.Sprintf("Metadata backend %q is a registered remote backend", metadata.Backend),
		PreflightDetails{MetadataBackend: metadata.Backend})
	var wire PreflightCheckResult
	if providerCheck.State == PreflightCheckFail {
		wire = NewPreflightCheckResult(PreflightCheckWireCompat, PreflightCheckWarn,
			fmt.Sprintf("server not probed; native store is already blocked by %s", providerCheck.ID),
			PreflightDetails{MetadataBackend: metadata.Backend})
	} else {
		wire = c.checkWireCompat(scope, metadata)
	}
	checks := append([]PreflightCheckResult{providerCheck, metadataCheck}, remoteNotConsultedChecks(metadata.Backend)...)
	checks = append(checks, wire)
	verdict := preflightVerdictForChecks(checks)
	result := PreflightResult{
		Verdict:             verdict,
		Scope:               scope,
		Checks:              checks,
		RepairSteps:         remoteRepairSteps(checks),
		NativeStoreEligible: verdict == PreflightVerdictEligible,
	}
	if verdict != PreflightVerdictEligible {
		result.Fallback = PreflightFallbackBdStore
		result.FallbackReason = preflightFallbackReason(checks)
	}
	return NewPreflightResult(result)
}

func (c PreflightChecker) checkWireCompat(scope string, metadata preflightMetadata) PreflightCheckResult {
	if c.WireHandshake == nil {
		return NewPreflightCheckResult(PreflightCheckWireCompat, PreflightCheckFail,
			"wire handshake reader is not configured", PreflightDetails{MetadataBackend: metadata.Backend})
	}
	handshake, err := c.WireHandshake(scope)
	return EvaluateWireCompat(metadata.Backend, metadata.ProjectID, handshake, err)
}

// EvaluateWireCompat is the wire_compat rule over one handshake read. It is
// exported so every surface that holds a cached handshake (doctor, a boot
// gate) reaches the same verdict as the store open.
func EvaluateWireCompat(backend, metadataProjectID string, handshake PreflightWireHandshake, err error) PreflightCheckResult {
	details := PreflightDetails{MetadataBackend: backend, MetadataProjectID: strings.TrimSpace(metadataProjectID)}
	fail := func(summary string) PreflightCheckResult {
		return NewPreflightCheckResult(PreflightCheckWireCompat, PreflightCheckFail, summary, details)
	}
	if err != nil {
		reason := PreflightWireUnreachable
		var wireErr *PreflightWireError
		if errors.As(err, &wireErr) && wireErr.Reason != "" {
			reason = wireErr.Reason
			if wireErr.Err != nil {
				err = wireErr.Err
			}
		}
		details.AdditionalDiagnostics = []PreflightDetailField{{Key: "wire_reason", Value: string(reason)}}
		return fail(fmt.Sprintf("server handshake failed (%s): %v", reason, err))
	}
	snap := handshake.Snapshot
	details.DBProjectID = snap.ProjectID
	details.BDVersion = snap.BdVersion
	details.AdditionalDiagnostics = []PreflightDetailField{
		{Key: "wire_revision", Value: fmt.Sprint(snap.WireRevision)},
		{Key: "api_version", Value: snap.APIVersion},
	}
	if snap.WireRevision < RemoteWireRevisionMin || snap.WireRevision > RemoteWireRevisionMax {
		return fail(fmt.Sprintf("server wire_revision %d is outside the linked client's range [%d, %d]",
			snap.WireRevision, RemoteWireRevisionMin, RemoteWireRevisionMax))
	}
	expected := strings.TrimSpace(handshake.TargetProjectID)
	if expected == "" {
		expected = strings.TrimSpace(metadataProjectID)
	}
	details.Expected = expected
	switch {
	case expected == "":
		return fail("no project id is pinned for this scope (neither the remote activation nor metadata.json names one)")
	case snap.ProjectID != expected:
		return fail(fmt.Sprintf("server owns project %q, scope expects %q", snap.ProjectID, expected))
	}
	advertised := make(map[string]bool, len(snap.Capabilities))
	for _, token := range snap.Capabilities {
		advertised[strings.TrimSpace(token)] = true
	}
	var missingRequired, missingOptional []string
	for _, row := range remoteCapabilityRequirements {
		if advertised[row.Token] {
			continue
		}
		switch row.Class {
		case RemoteCapabilityRequired:
			missingRequired = append(missingRequired, row.Token)
		default:
			missingOptional = append(missingOptional, fmt.Sprintf("%s (fallback: %s)", row.Token, row.Fallback))
		}
	}
	if len(missingRequired) > 0 {
		return fail(fmt.Sprintf("server does not advertise required capabilities: %s", strings.Join(missingRequired, ", ")))
	}
	if len(missingOptional) > 0 {
		return NewPreflightCheckResult(PreflightCheckWireCompat, PreflightCheckWarn,
			fmt.Sprintf("server lacks optional capabilities: %s", strings.Join(missingOptional, ", ")), details)
	}
	return NewPreflightCheckResult(PreflightCheckWireCompat, PreflightCheckPass,
		fmt.Sprintf("server wire_revision %d, project %q, all required capabilities advertised", snap.WireRevision, snap.ProjectID), details)
}

func remoteRepairSteps(checks []PreflightCheckResult) []PreflightRepairStep {
	for _, check := range checks {
		if check.ID == PreflightCheckWireCompat && check.State == PreflightCheckFail {
			return []PreflightRepairStep{{
				CheckID:  check.ID,
				Priority: PreflightRepairCritical,
				Command:  "bd connect",
				Note:     `Point the scope at the server that owns it and upgrade a server that lacks required capabilities; set beads.native_transport = "off" to use the bd CLI meanwhile.`,
			}}
		}
	}
	return nil
}
