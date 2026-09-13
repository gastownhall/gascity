package api

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// webhookSourceAllowed enforces a hook's operator-declared allowed_cidrs source
// allowlist (security review finding #2 — the documented control was previously a
// no-op). It matches the DIRECT connection address (RemoteAddr) against the
// allowlist and deliberately does NOT trust X-Forwarded-For, mirroring the
// supervisor's remote_addr_class policy, which classifies the peer address and
// never a forwarded header. An operator using this control must therefore deploy
// so the supervisor observes the real source address (e.g. via the PROXY
// protocol). An empty allowlist is a no-op; a malformed allowlist is an operator
// fault that fails CLOSED (503), never open. It returns false once it has written
// the response.
func (s *Server) webhookSourceAllowed(w http.ResponseWriter, r *http.Request, req webhookRequest) bool {
	cidrs := req.hook.Verify.AllowedCIDRs
	if len(cidrs) == 0 {
		return true
	}
	prefixes, err := config.ParseWebhookCIDRs(cidrs)
	if err != nil {
		// Load-time validation should reject a malformed allowlist; if one still
		// reaches here, fail closed rather than silently skipping the control. This
		// runs before the limiter, so the fault is non-evented and logged one-shot
		// (rejectWebhookAccessOperatorFault) to avoid a CWE-400 flood amplifier.
		s.rejectWebhookAccessOperatorFault(w, req.hook.Name, fmt.Sprintf("allowed_cidrs invalid: %v", err))
		return false
	}
	if ip, ok := webhookRemoteIP(r.RemoteAddr); ok {
		for _, p := range prefixes {
			if p.Contains(ip) {
				return true
			}
		}
	}
	// Off-allowlist source → 403, deliberately NON-evented. Like the perimeter and
	// rate-limit rejects, the caller fully controls their source address, so this
	// gate runs before the limiter (it must not consume a delivery token) and
	// eventing it per request would be the flood amplifier the receiver avoids.
	problemWebhookForbiddenSource.writeTo(w)
	return false
}

// webhookRemoteIP extracts the connection's IP from a RemoteAddr ("host:port" or
// a bare host), returning ok=false when it cannot be parsed — which the caller
// treats as not-allowed (fail closed).
func webhookRemoteIP(remoteAddr string) (netip.Addr, bool) {
	host := strings.TrimSpace(remoteAddr)
	if host == "" {
		return netip.Addr{}, false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	// Unmap so a 4-in-6 form (::ffff:1.2.3.4) matches an IPv4 allowlist prefix.
	return addr.Unmap(), true
}

// webhookBearerAllowed enforces a hook's optional operator-owned bearer_env token
// alongside the signature (security review finding #2 — the documented control
// was previously a no-op). When bearer_env is set, the resolved token must be
// present and equal (constant-time) to the request's "Authorization: Bearer
// <token>". bearer_env is validated at config load to live in the GC_WEBHOOK_*
// operator namespace, so a pack cannot point it at an ambient variable. An
// unset/empty bearer_env variable is an operator fault (503, fail closed); a
// missing or mismatched token is a 401. Empty bearer_env is a no-op.
func (s *Server) webhookBearerAllowed(w http.ResponseWriter, r *http.Request, req webhookRequest) bool {
	env := strings.TrimSpace(req.hook.Verify.BearerEnv)
	if env == "" {
		return true
	}
	expected, ok := os.LookupEnv(env)
	if !ok || strings.TrimSpace(expected) == "" {
		// Unset/empty bearer_env is an operator fault (503, fail closed). It runs
		// before the limiter, so it is non-evented and logged one-shot
		// (rejectWebhookAccessOperatorFault) to avoid a CWE-400 flood amplifier.
		s.rejectWebhookAccessOperatorFault(w, req.hook.Name, fmt.Sprintf("bearer_env %q is unset or empty", env))
		return false
	}
	provided := bearerToken(r.Header.Get("Authorization"))
	if subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
		// Missing/wrong bearer → 401, deliberately NON-evented. The caller fully
		// controls the Authorization header, so this gate runs before the limiter (it
		// must not consume a delivery token) and eventing it per request would amplify
		// a flood. An unset/empty bearer_env above is an operator fault (503) that is
		// likewise pre-limiter, so it too stays non-evented (one-shot logged) rather
		// than amplifying a flood into per-request writes.
		problemWebhookUnauthorized.writeTo(w)
		return false
	}
	return true
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header,
// returning "" when the header is absent or is not a bearer credential. The
// scheme name is matched case-insensitively per RFC 7235.
func bearerToken(authHeader string) string {
	const scheme = "Bearer "
	h := strings.TrimSpace(authHeader)
	if len(h) >= len(scheme) && strings.EqualFold(h[:len(scheme)], scheme) {
		return strings.TrimSpace(h[len(scheme):])
	}
	return ""
}
