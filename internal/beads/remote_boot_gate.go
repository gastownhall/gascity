package beads

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	bdhttp "github.com/steveyegge/beads/backend/http"

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
//
// The gate refuses only what no retry can fix (a STRUCTURAL failure): a
// missing required capability, a project mismatch, an api_version or
// wire_revision the linked client does not speak, an unconnected scope, or a
// missing or rejected credential (401/403). A server that cannot be reached at
// boot (unreachable, DNS, connection refused, a handshake timeout, a 5xx) is
// an outage, not a mismatch: the gate WARNs naming the scope and the city
// starts, and the store open deals with the outage when it next dials.
// Refusing there would turn one rig's server outage into the whole city
// failing to start, and the supervisor would back off the city as if it were
// misconfigured.

// RemoteBootGateCheck is the check id the gate and its doctor surface report:
// the gate IS wire_compat, evaluated at boot.
const RemoteBootGateCheck = contract.PreflightCheckWireCompat

// RemoteBootGateRefusalPrefix starts every *RemoteCapabilityGateError's text.
// The supervisor classifies an init failure by its message, so it reads a
// gate refusal (always structural) by this prefix.
const RemoteBootGateRefusalPrefix = "boot capability gate ("

// IsRemoteBootGateRefusalMessage reports whether msg carries a boot gate
// refusal (see RemoteBootGateRefusalPrefix).
func IsRemoteBootGateRefusalMessage(msg string) bool {
	return strings.Contains(msg, RemoteBootGateRefusalPrefix)
}

// RemoteBootGateFailure classifies a structural boot gate refusal, which
// decides the remedy its text offers.
type RemoteBootGateFailure string

const (
	// RemoteBootGateCapability is a REQUIRED capability the server lacks.
	RemoteBootGateCapability RemoteBootGateFailure = "capability"
	// RemoteBootGateVersion is an api_version or wire_revision the linked
	// client does not speak.
	RemoteBootGateVersion RemoteBootGateFailure = "version"
	// RemoteBootGateProject is a server owning another project, or a scope
	// pinning none.
	RemoteBootGateProject RemoteBootGateFailure = "project"
	// RemoteBootGateNotConnected is a scope with no usable activation.
	RemoteBootGateNotConnected RemoteBootGateFailure = "not_connected"
	// RemoteBootGateCredential is a missing, unresolvable or rejected
	// (401/403) credential.
	RemoteBootGateCredential RemoteBootGateFailure = "credential"
	// RemoteBootGateHandshake is any other answer that is not an outage: an
	// untrusted certificate, a redirect, a handshake the client refused.
	RemoteBootGateHandshake RemoteBootGateFailure = "handshake"
)

// RemoteCapabilityGateError is the boot gate's refusal for one remote scope.
// It is only ever a structural failure (see the file comment).
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
	// Failure classifies the refusal. Empty reads as a capability or version
	// refusal.
	Failure RemoteBootGateFailure
}

// Error implements error. It names the check, the scope and the remedy for
// this kind of refusal. Only a server that cannot serve the NATIVE store
// (a capability or version gap) offers native_transport = "off": the bd CLI
// fails the same way on a wrong project, a missing activation or a refused
// credential.
func (e *RemoteCapabilityGateError) Error() string {
	if e == nil {
		return ""
	}
	msg := fmt.Sprintf(RemoteBootGateRefusalPrefix+"%s) refused the native store for remote beads backend %q at %s: %s",
		e.Check, e.Backend, e.ScopeRoot, e.Summary)
	switch e.Failure {
	case RemoteBootGateCredential:
		return msg + "; fix the scope's credential ([beads] credential, the rig's beads_credential, or bd's credential ladder), then start again"
	case RemoteBootGateProject, RemoteBootGateNotConnected:
		return msg + "; point the scope at the server that owns its project (gc storage connect, --retarget to re-pin it), then start again"
	case RemoteBootGateHandshake:
		return msg + "; fix the server or its activation (gc storage connect), then start again"
	}
	if len(e.MissingRequired) > 0 {
		msg += "; upgrade the bd serve behind it to one advertising " + strings.Join(e.MissingRequired, ", ")
	} else {
		msg += "; upgrade the bd serve behind it (or gc) so the two speak one api_version and wire revision"
	}
	return msg + fmt.Sprintf(`, or set beads.native_transport = %q in city.toml to run this city through the bd CLI`, string(NativeTransportOff))
}

// RemoteScopeUnreachableError is the boot gate's WARNING for one remote scope
// whose server could not be reached at boot (a transport failure, never a
// structural one). The gate does not refuse on it: callers report it, naming
// the scope, and start the city.
type RemoteScopeUnreachableError struct {
	// ScopeRoot is the scope (city or rig) whose server was not reached.
	ScopeRoot string
	// ActivationRoot is the scope whose .beads holds the remote activation.
	ActivationRoot string
	// Backend is the registered remote backend the scope's metadata names.
	Backend string
	// Summary is the handshake failure.
	Summary string
	// Err is the handshake error.
	Err error
}

// Error implements error.
func (e *RemoteScopeUnreachableError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("remote beads backend %q for %s is unreachable at boot: %s; starting anyway, and this scope's store reports the outage until its server answers",
		e.Backend, e.ScopeRoot, e.Summary)
}

// Unwrap returns the handshake error.
func (e *RemoteScopeUnreachableError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// CheckRemoteScopeBootGate evaluates the boot gate for one scope. remote is
// false (and the result zero) for a scope whose metadata does not select a
// registered remote backend: the gate has nothing to say about it. For a
// remote scope it returns the wire_compat verdict and, when that verdict is
// a structural FAIL, a *RemoteCapabilityGateError (refuse the start). A
// server that cannot be reached is a WARN verdict with a
// *RemoteScopeUnreachableError, which the caller reports and starts past. A
// WARN with no error (an optional capability missing) starts: the store
// records the fallback.
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
	refuse := func(check, summary string, missing []string, failure RemoteBootGateFailure) error {
		return &RemoteCapabilityGateError{
			ScopeRoot:       scopeRoot,
			ActivationRoot:  activationRoot,
			Backend:         verdict.Backend,
			Check:           check,
			Summary:         summary,
			MissingRequired: missing,
			Failure:         failure,
		}
	}
	plan, planErr := remoteOpenPlanFor(ctx, cityPath, scopeRoot, activationRoot)
	if planErr != nil {
		result = contract.NewPreflightCheckResult(RemoteBootGateCheck, contract.PreflightCheckFail,
			"remote credential: "+planErr.Error(), contract.PreflightDetails{MetadataBackend: verdict.Backend})
		return result, true, refuse(remoteCredentialGate, planErr.Error(), nil, RemoteBootGateCredential)
	}
	handshake, hsErr := remoteWireHandshakeWith(activationRoot, plan)
	if hsErr != nil && remoteHandshakeTransportFailure(hsErr) {
		summary := "server not reached: " + hsErr.Error()
		result = contract.NewPreflightCheckResult(RemoteBootGateCheck, contract.PreflightCheckWarn, summary,
			contract.PreflightDetails{MetadataBackend: verdict.Backend})
		return result, true, &RemoteScopeUnreachableError{
			ScopeRoot:      scopeRoot,
			ActivationRoot: activationRoot,
			Backend:        verdict.Backend,
			Summary:        summary,
			Err:            hsErr,
		}
	}
	result = contract.EvaluateWireCompat(verdict.Backend, readMetadataProjectID(activationRoot), handshake, hsErr)
	if result.State != contract.PreflightCheckFail {
		return result, true, nil
	}
	var missing []string
	if hsErr == nil {
		missing, _ = contract.MissingRemoteCapabilities(handshake.Snapshot.Capabilities)
	}
	return result, true, refuse(string(RemoteBootGateCheck), result.Summary, missing, remoteBootGateFailureOf(handshake, hsErr, missing))
}

// remoteBootGateFailureOf classifies a structural wire_compat FAIL.
func remoteBootGateFailureOf(handshake contract.PreflightWireHandshake, hsErr error, missing []string) RemoteBootGateFailure {
	if hsErr != nil {
		var wireErr *contract.PreflightWireError
		if !errors.As(hsErr, &wireErr) {
			return RemoteBootGateHandshake
		}
		switch wireErr.Reason {
		case contract.PreflightWireProjectMismatch:
			return RemoteBootGateProject
		case contract.PreflightWireAPIVersion:
			return RemoteBootGateVersion
		case contract.PreflightWireCapability:
			return RemoteBootGateCapability
		case contract.PreflightWireUnauthenticated:
			return RemoteBootGateCredential
		case contract.PreflightWireNotConnected:
			return RemoteBootGateNotConnected
		}
		return RemoteBootGateHandshake
	}
	snap := handshake.Snapshot
	switch {
	case len(missing) > 0:
		return RemoteBootGateCapability
	case snap.WireRevision < contract.RemoteWireRevisionMin || snap.WireRevision > contract.RemoteWireRevisionMax:
		return RemoteBootGateVersion
	}
	return RemoteBootGateProject
}

// remoteHandshakeTransportFailure reports whether a failed handshake is an
// OUTAGE (the server was not reached, or answered 5xx) rather than a
// structural answer. It recognizes transport failures positively, by type:
// anything it does not recognize stays structural, so an unclassified
// refusal still stops the start rather than being waved through as a blip.
// Certificate and TLS trust failures are configuration, never an outage.
func remoteHandshakeTransportFailure(err error) bool {
	if err == nil {
		return false
	}
	var wireErr *contract.PreflightWireError
	if errors.As(err, &wireErr) && wireErr.Reason != contract.PreflightWireUnreachable {
		return false
	}
	var (
		unknownAuthority x509.UnknownAuthorityError
		hostname         x509.HostnameError
		invalid          x509.CertificateInvalidError
		verification     *tls.CertificateVerificationError
		alert            tls.AlertError
		record           tls.RecordHeaderError
	)
	if errors.As(err, &unknownAuthority) || errors.As(err, &hostname) || errors.As(err, &invalid) ||
		errors.As(err, &verification) || errors.As(err, &alert) || errors.As(err, &record) {
		return false
	}
	var problem *bdhttp.ProblemError
	if errors.As(err, &problem) {
		return problem.Status >= 500
	}
	if errors.Is(err, bdhttp.ErrServerFault) || errors.Is(err, bdhttp.ErrBusy) || errors.Is(err, bdhttp.ErrDBUnavailable) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var (
		dnsErr *net.DNSError
		opErr  *net.OpError
	)
	if errors.As(err, &dnsErr) || errors.As(err, &opErr) {
		return true
	}
	for _, errno := range []syscall.Errno{syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EHOSTUNREACH, syscall.ENETUNREACH, syscall.ETIMEDOUT} {
		if errors.Is(err, errno) {
			return true
		}
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
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
