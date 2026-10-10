package beads

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
)

// The boot capability gate (DESIGN C5 G10).
//
// wire_compat already runs on every native open of a remote scope, so a
// server that cannot serve the native store refuses the open. Without a boot
// gate that refusal arrives late: at the first store open a city makes,
// which on a supervisor is after the city has published itself, opened its
// event log and started its controller. The gate evaluates the same rule
// (contract.EvaluateWireCompat over the same cached handshake, dialed with the
// same per-scope plan the store open uses) once, at boot, for every remote
// scope the city will open natively, and refuses to start the city with a
// typed, actionable error naming the scope, the check and the missing tokens.

// RemoteBootGateCheck is the check id the gate and its doctor surface report:
// the gate IS wire_compat, evaluated at boot.
const RemoteBootGateCheck = contract.PreflightCheckWireCompat

// RemoteCapabilityGateError is the boot gate's refusal for one remote scope.
type RemoteCapabilityGateError struct {
	// ScopeRoot is the scope (city or rig) whose native store was refused.
	ScopeRoot string
	// ActivationRoot is the scope whose .beads holds the remote activation.
	ActivationRoot string
	// Backend is the registered remote backend the scope's metadata names.
	Backend string
	// Check is the failed check's id (wire_compat), or the credential gate.
	Check string
	// Summary is the check's own verdict text.
	Summary string
	// MissingRequired lists the REQUIRED capability tokens the server lacks,
	// when that is why it failed.
	MissingRequired []string
}

// Error implements error. It names the check, the scope and both remedies.
func (e *RemoteCapabilityGateError) Error() string {
	if e == nil {
		return ""
	}
	msg := fmt.Sprintf("boot capability gate (%s) refused the native store for remote beads backend %q at %s: %s",
		e.Check, e.Backend, e.ScopeRoot, e.Summary)
	if len(e.MissingRequired) > 0 {
		msg += "; upgrade the bd serve behind it to one advertising " + strings.Join(e.MissingRequired, ", ")
	} else {
		msg += "; fix the server or its activation (gc storage connect), then start again"
	}
	return msg + fmt.Sprintf(`, or set beads.native_transport = %q in city.toml to run this city through the bd CLI`, string(NativeTransportOff))
}

// CheckRemoteScopeBootGate evaluates the boot gate for one scope. remote is
// false (and the result zero) for a scope whose metadata does not select a
// registered remote backend: the gate has nothing to say about it. For a
// remote scope it returns the wire_compat verdict and, when that verdict is
// FAIL, a *RemoteCapabilityGateError. A WARN (an optional capability missing)
// starts: the store records the fallback.
//
// It dials with the plan the native open uses (remoteOpenPlanFor), so the
// per-scope credential and plaintext grant are the store's, and it shares the
// per-process handshake cache, so the store open right after a passing gate
// does not dial the handshake again.
func CheckRemoteScopeBootGate(ctx context.Context, cityPath, scopeRoot string) (result contract.PreflightCheckResult, remote bool, err error) {
	verdict := decideMetadataBackend(scopeRoot, cityPath)
	if verdict.Route != metadataBackendRouteRemote {
		return contract.PreflightCheckResult{}, false, nil
	}
	activationRoot := verdict.ActivationRoot
	if activationRoot == "" {
		activationRoot = scopeRoot
	}
	refuse := func(check, summary string, missing []string) error {
		return &RemoteCapabilityGateError{
			ScopeRoot:       scopeRoot,
			ActivationRoot:  activationRoot,
			Backend:         verdict.Backend,
			Check:           check,
			Summary:         summary,
			MissingRequired: missing,
		}
	}
	plan, planErr := remoteOpenPlanFor(ctx, cityPath, scopeRoot, activationRoot)
	if planErr != nil {
		result = contract.NewPreflightCheckResult(RemoteBootGateCheck, contract.PreflightCheckFail,
			"remote credential: "+planErr.Error(), contract.PreflightDetails{MetadataBackend: verdict.Backend})
		return result, true, refuse(remoteCredentialGate, planErr.Error(), nil)
	}
	handshake, hsErr := remoteWireHandshakeWith(activationRoot, plan)
	result = contract.EvaluateWireCompat(verdict.Backend, readMetadataProjectID(activationRoot), handshake, hsErr)
	if result.State != contract.PreflightCheckFail {
		return result, true, nil
	}
	var missing []string
	if hsErr == nil {
		missing, _ = contract.MissingRemoteCapabilities(handshake.Snapshot.Capabilities)
	}
	return result, true, refuse(string(RemoteBootGateCheck), result.Summary, missing)
}

// readMetadataProjectID reads metadata.json's project_id for wire_compat's
// fallback identity (the activation's pinned project wins when it has one).
func readMetadataProjectID(scopeRoot string) string {
	data, err := os.ReadFile(filepath.Join(scopeRoot, ".beads", "metadata.json"))
	if err != nil {
		return ""
	}
	var meta struct {
		ProjectID string `json:"project_id"`
	}
	if json.Unmarshal(data, &meta) != nil {
		return ""
	}
	return strings.TrimSpace(meta.ProjectID)
}
