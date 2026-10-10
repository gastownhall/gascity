package beads

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	bdhttp "github.com/steveyegge/beads/backend/http"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// Attaching a city or rig scope to an HTTP bd serve (DESIGN C5 G10:
// `gc storage connect`).
//
// gc never writes the activation itself: beads owns both files (metadata.json's
// backend selection and the per-user http_target.json sidecar), and
// bdhttp.Attach is the one door that writes them. What gc adds is the part
// bd connect cannot know: the scope's per-city / per-rig credential (city.toml
// [beads] credential, rigs.beads_credential) for the verifying handshake, and
// the boot gate's verdict BEFORE anything is written, so a scope is never
// attached to a server its native store would then refuse at boot.
//
// The order is bdhttp.Connect's ("handshake, then attach"), split so the
// wire_compat verdict sits between the two.

// RemoteConnectRequest names the scope to attach and the server to attach it
// to.
type RemoteConnectRequest struct {
	// CityPath is the city the scope belongs to (for its credential config).
	CityPath string
	// ScopeRoot is the city or rig directory whose .beads is activated.
	ScopeRoot string
	// URL is the bd serve mount root (http or https).
	URL string
	// ProjectID pins the project the server must own. Empty pins whatever
	// project the server reports, as bd connect does.
	ProjectID string
	// CAFile is an absolute PEM path that becomes this target's whole trusted
	// root pool (bd connect --ca-file).
	CAFile string
	// AllowPlaintext grants a bearer over plain http to a non-loopback server
	// when the scope has NO configured credential (bd connect
	// --allow-plaintext). A configured credential's grant is its own
	// allow_insecure_credential, never this flag.
	AllowPlaintext bool
	// ConvertWorkspace permits switching a scope whose metadata selects a
	// local backend. bdhttp.Attach records the previous backend so
	// `bd connect --clear` can restore it.
	ConvertWorkspace bool
	// Retarget permits re-pinning a scope already attached to another server
	// or project.
	Retarget bool
}

// RemoteConnectResult reports what ConnectRemoteScope did.
type RemoteConnectResult struct {
	ScopeRoot string
	BeadsDir  string
	// Backend is the backend metadata.json now selects.
	Backend string
	// URL is the attached server, redacted.
	URL string
	// Server is what the server said at the verifying handshake.
	Server contract.PreflightWireSnapshot
	// WireCompat is the boot gate's verdict on that handshake (PASS or WARN;
	// a FAIL refuses before anything is written).
	WireCompat contract.PreflightCheckResult
	// Credential names the credential the handshake used: the configured
	// source (redacted) or bd's ambient ladder.
	Credential string
	// Changed is false when the scope was already attached to this server
	// and project.
	Changed bool
}

// ErrRemoteConnectRefused is matched by every ConnectRemoteScope refusal
// that is a policy decision rather than a failure to reach the server.
var ErrRemoteConnectRefused = errors.New("remote beads connect refused")

// ConnectRemoteScope verifies the server with the scope's credential, checks
// it against the boot gate's class requirement table, and only then attaches
// the scope through bdhttp.Attach. After it returns, the scope opens natively
// through the remote route (decideMetadataBackend) with no further setup.
func ConnectRemoteScope(ctx context.Context, req RemoteConnectRequest) (RemoteConnectResult, error) {
	scopeRoot := filepath.Clean(strings.TrimSpace(req.ScopeRoot))
	if scopeRoot == "" || scopeRoot == "." {
		return RemoteConnectResult{}, fmt.Errorf("%w: no scope directory", ErrRemoteConnectRefused)
	}
	refuse := func(format string, args ...any) (RemoteConnectResult, error) {
		return RemoteConnectResult{}, fmt.Errorf("%w: %s", ErrRemoteConnectRefused, fmt.Sprintf(format, args...))
	}
	base, err := url.Parse(strings.TrimSpace(req.URL))
	if err != nil || base.Host == "" || (!strings.EqualFold(base.Scheme, "http") && !strings.EqualFold(base.Scheme, "https")) {
		return refuse("server url %q: want http(s)://host[:port][/mount]", req.URL)
	}
	if base.User != nil {
		return refuse("server url must not carry userinfo; configure [beads] credential instead")
	}
	if ca := strings.TrimSpace(req.CAFile); ca != "" && !filepath.IsAbs(ca) {
		return refuse("ca file %q must be an absolute path", ca)
	}
	beadsDir := filepath.Join(scopeRoot, ".beads")
	target := bdhttp.Target{BaseURL: base, ExpectProjectID: strings.TrimSpace(req.ProjectID), CAFile: strings.TrimSpace(req.CAFile)}

	// What the scope selects today decides whether attaching it is a switch
	// that needs consent.
	current, named, readErr := contract.ReadMetadataBackend(fsys.OSFS{}, filepath.Join(beadsDir, "metadata.json"))
	current = strings.TrimSpace(current)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return refuse("reading %s: %v", filepath.Join(beadsDir, "metadata.json"), readErr)
	}
	changed := true
	switch {
	case named && contract.BackendIsRemote(current):
		prior, priorErr := bdhttp.LoadTarget(beadsDir)
		if priorErr == nil && prior.BaseURL != nil {
			sameServer := remoteOrigin(prior.BaseURL) == remoteOrigin(base) && strings.TrimRight(prior.BaseURL.Path, "/") == strings.TrimRight(base.Path, "/")
			sameProject := target.ExpectProjectID == "" || target.ExpectProjectID == prior.ExpectProjectID
			if !sameServer || !sameProject {
				if !req.Retarget {
					return refuse("%s is already attached to %s (project %q); pass --retarget to re-pin it", scopeRoot, prior.BaseURL.Redacted(), prior.ExpectProjectID)
				}
			} else {
				if target.ExpectProjectID == "" {
					target.ExpectProjectID = prior.ExpectProjectID
				}
				changed = prior.CAFile != target.CAFile
			}
		}
	case req.ConvertWorkspace:
	case named:
		return refuse("%s selects the %q backend; attaching it to a server switches where its beads live, so pass --convert-workspace to do that (bd connect --clear restores %q)", scopeRoot, current, current)
	default:
		// metadata.json without a backend marker is the default (Dolt)
		// backend, which is a switch all the same.
		if _, statErr := os.Stat(filepath.Join(beadsDir, "metadata.json")); statErr == nil {
			return refuse("%s already has a local beads workspace; attaching it to a server switches where its beads live, so pass --convert-workspace to do that", scopeRoot)
		}
	}

	opts := bdhttp.Options{UserAgent: remoteUserAgent}
	if seam := nativeOpenOptions(beadsDir); seam.HTTPClient != nil || seam.UserAgent != "" {
		opts.HTTPClient = seam.HTTPClient
		if seam.UserAgent != "" {
			opts.UserAgent = seam.UserAgent
		}
	}
	credential, err := connectCredential(ctx, req.CityPath, scopeRoot, &target, req.AllowPlaintext)
	if err != nil {
		return RemoteConnectResult{}, err
	}
	opts.Credential = credential.provider

	snapshot, err := remoteHandshakeDial(ctx, beadsDir, target, opts)
	if err != nil {
		return RemoteConnectResult{}, &contract.PreflightWireError{Reason: classifyRemoteHandshakeError(err), Err: err}
	}
	if snapshot == nil {
		return RemoteConnectResult{}, &contract.PreflightWireError{Reason: contract.PreflightWireUnreachable, Err: errors.New("server returned no handshake")}
	}
	if target.ExpectProjectID == "" {
		target.ExpectProjectID = snapshot.ProjectID
	}
	server := contract.PreflightWireSnapshot{
		APIVersion:   snapshot.APIVersion,
		BdVersion:    snapshot.BdVersion,
		WireRevision: snapshot.WireRevision,
		ProjectID:    snapshot.ProjectID,
		Capabilities: append([]string(nil), snapshot.Capabilities...),
	}
	verdict := contract.EvaluateWireCompat("", "", contract.PreflightWireHandshake{Snapshot: server, TargetProjectID: target.ExpectProjectID}, nil)
	if verdict.State == contract.PreflightCheckFail {
		missing, _ := contract.MissingRemoteCapabilities(server.Capabilities)
		return RemoteConnectResult{}, &RemoteCapabilityGateError{
			ScopeRoot:       scopeRoot,
			ActivationRoot:  scopeRoot,
			Check:           string(RemoteBootGateCheck),
			Summary:         verdict.Summary + " (nothing was written)",
			MissingRequired: missing,
		}
	}

	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		return RemoteConnectResult{}, fmt.Errorf("creating %s: %w", beadsDir, err)
	}
	if err := bdhttp.Attach(beadsDir, target); err != nil {
		return RemoteConnectResult{}, fmt.Errorf("attaching %s: %w", beadsDir, err)
	}
	forgetRemoteHandshakes(beadsDir)
	backend, _, _ := contract.ReadMetadataBackend(fsys.OSFS{}, filepath.Join(beadsDir, "metadata.json"))
	return RemoteConnectResult{
		ScopeRoot:  scopeRoot,
		BeadsDir:   beadsDir,
		Backend:    strings.TrimSpace(backend),
		URL:        base.Redacted(),
		Server:     server,
		WireCompat: verdict,
		Credential: credential.label,
		Changed:    changed,
	}, nil
}

type connectCredentialChoice struct {
	provider bdhttp.CredentialProvider
	label    string
}

// connectCredential picks the credential the verifying handshake sends, by
// the rules the scope's native opens will follow once it is attached
// (prepareScopeRemoteCredential): a configured credential alone, never to a
// server other than the city's when it is the city's, never over plain http
// to a non-loopback host without allow_insecure_credential. With none
// configured, bd's ambient ladder for that host, resolved now.
func connectCredential(ctx context.Context, cityPath, scopeRoot string, target *bdhttp.Target, allowPlaintext bool) (connectCredentialChoice, error) {
	lookup := currentRemoteCredentialLookup()
	if lookup != nil {
		cfg, ok, err := lookup(cityPath, scopeRoot)
		if err != nil {
			return connectCredentialChoice{}, &RemoteCredentialError{ScopeRoot: scopeRoot, Reason: RemoteCredentialReasonLookup, Err: err}
		}
		if ok && !cfg.Source.IsZero() {
			fail := func(reason RemoteCredentialReason, err error) (connectCredentialChoice, error) {
				return connectCredentialChoice{}, &RemoteCredentialError{ScopeRoot: scopeRoot, Scope: cfg.Scope, Source: cfg.Source.String(), Reason: reason, Err: err}
			}
			if cfg.FromCity && strings.TrimSpace(cityPath) != "" && !sameScopeRoot(scopeRoot, cityPath) {
				cityTarget, cityErr := bdhttp.LoadTarget(filepath.Join(cityPath, ".beads"))
				if cityErr != nil || cityTarget.BaseURL == nil || remoteOrigin(cityTarget.BaseURL) != remoteOrigin(target.BaseURL) {
					return fail(RemoteCredentialReasonServerMismatch, fmt.Errorf("%w: this rig would attach to %s, the city is attached to %s; set the rig's beads_credential",
						ErrRemoteCredentialServerMismatch, remoteOrigin(target.BaseURL), cityOriginForMessage(cityTarget, cityErr)))
				}
			}
			if remotePlaintextNonLoopback(target.BaseURL) && !cfg.AllowInsecure {
				return fail(RemoteCredentialReasonInsecure, fmt.Errorf("%w %s; serve it over https, bind it to loopback, or set allow_insecure_credential = true for this scope",
					ErrRemoteCredentialInsecure, remoteOrigin(target.BaseURL)))
			}
			target.AllowInsecureCredential = cfg.AllowInsecure
			provider := &sourceCredential{source: cfg.Source, dir: cfg.Dir, maxAge: scopeCredentialMaxAge(cfg.Source)}
			if _, err := provider.current(ctx); err != nil {
				return fail(RemoteCredentialReasonResolve, err)
			}
			return connectCredentialChoice{provider: provider, label: cfg.Scope + " credential " + cfg.Source.String()}, nil
		}
	}
	target.AllowInsecureCredential = allowPlaintext
	return connectCredentialChoice{provider: newResolvedAmbientCredential(ctx, target.BaseURL), label: "bd credential ladder"}, nil
}

// forgetRemoteHandshakes drops every cached handshake of one activation, so
// the first open after a (re)attach dials the server it now names.
func forgetRemoteHandshakes(beadsDir string) {
	prefix := beadsDir + "\x00"
	remoteHandshakeMu.Lock()
	defer remoteHandshakeMu.Unlock()
	for key := range remoteHandshakeCache {
		if strings.HasPrefix(key, prefix) {
			delete(remoteHandshakeCache, key)
		}
	}
}
