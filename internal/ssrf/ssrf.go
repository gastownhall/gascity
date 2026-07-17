// Package ssrf is the shared server-side request-forgery fence for git network
// operations whose remote URL is supplied by an API caller. It classifies a
// host (or a raw inet_aton literal) as internal-or-public so a caller can reject
// a fetch aimed at loopback, RFC1918/ULA, link-local, or a cloud-metadata
// endpoint before spawning git.
//
// It is the single implementation shared by the pack-import fence
// (internal/api/pack_source_policy.go) and the rig-clone provisioning path
// (C3/G15). Keeping one fence avoids the two copies drifting — a security
// regression the duplication would invite.
//
// The fence is one layer of defense in depth. On its own, EnsurePublicHost does
// NOT close the DNS-rebinding TOCTOU window: git re-resolves the host at fetch
// time, so a name that resolves public here can resolve internal at the fetch.
// The rig-clone path closes that residual with ResolvePublicHostStrict, which
// returns the fence-approved addresses so the caller can PIN them at connection
// time (git http.curloptResolve); git then connects to exactly those addresses
// instead of re-resolving the name, and TLS still verifies against the original
// hostname. Redirect refusal and transport constraints at the git subprocess are
// the additional in-depth layers.
package ssrf

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// ErrBlockedHost wraps every fence rejection for a host that names or resolves
// to an internal destination. Callers match it with errors.Is to map the block
// onto their own API/CLI error surface.
var ErrBlockedHost = errors.New("host is an internal or non-public address")

// ErrEmptyHost is returned by EnsurePublicHost when the host is blank. It is
// distinct from ErrBlockedHost so a caller can surface a "could not determine a
// host" message rather than a "blocked" one.
var ErrEmptyHost = errors.New("host is empty")

// HostResolver resolves a hostname to its IP addresses for the fence. It is a
// package var so tests can stub DNS without touching the network; the default
// uses the process resolver. Callers must save and restore it around a stub.
var HostResolver = func(host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(context.Background(), host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, len(addrs))
	for i, a := range addrs {
		ips[i] = a.IP
	}
	return ips, nil
}

// EnsurePublicHost rejects a host that names or resolves to an internal
// destination: loopback, RFC1918 or IPv6 unique-local, link-local (including the
// 169.254.169.254 cloud-metadata endpoint), interface-local, or the unspecified
// address. It also decodes the legacy inet_aton literals git's C resolver
// accepts (hex, octal, and dotless-integer forms) so an encoded internal target
// cannot slip past.
//
// Hostnames that neither parse as a literal nor resolve to an internal address
// are allowed. A resolution error is NOT treated as a block: the subsequent git
// fetch performs its own resolution and surfaces the failure there, and the
// fence only blocks on a positively-internal address (matching the pack fence's
// long-standing behavior). Use EnsurePublicHostStrict on a fresh network surface
// (the rig-clone path) where a resolution error must fail closed.
//
// The returned error wraps ErrEmptyHost for a blank host and ErrBlockedHost for
// an internal one.
func EnsurePublicHost(host string) error {
	return ensurePublicHost(host, false)
}

// EnsurePublicHostStrict is EnsurePublicHost with fail-closed resolution: a DNS
// resolution error is treated as a block (wrapping ErrBlockedHost), not allowed
// through. It is the variant the rig-clone provisioning path uses (C3/G15),
// because a clone is a fresh SSRF surface where an attacker can force a SERVFAIL
// (or otherwise poison resolution) to slip past the fail-open fence and then
// win the DNS-rebinding TOCTOU at git's own re-resolution. The pack-import fence
// stays on the fail-open EnsurePublicHost — its long-standing behavior — so this
// hardening is scoped to the new clone surface only.
func EnsurePublicHostStrict(host string) error {
	_, err := resolvePublicHost(host, true)
	return err
}

// ResolvePublicHostStrict is EnsurePublicHostStrict plus the fence-approved
// addresses to pin. It returns the resolved public IPs for a DNS name so the
// rig-clone path can pass them to git via http.curloptResolve, closing the
// DNS-rebinding TOCTOU: git connects to exactly these already-validated
// addresses instead of re-resolving the name at fetch time, while TLS still
// verifies against the hostname. It returns an EMPTY slice (with a nil error)
// for a literal or encoded-literal IP host — there is no name to pin, and the
// URL already names the address git will use. Any resolution error or internal
// address fails closed exactly as EnsurePublicHostStrict does.
func ResolvePublicHostStrict(host string) ([]net.IP, error) {
	return resolvePublicHost(host, true)
}

// ensurePublicHost is the shared verdict-only entry for EnsurePublicHost
// (failClosed false) and EnsurePublicHostStrict (failClosed true).
func ensurePublicHost(host string, failClosed bool) error {
	_, err := resolvePublicHost(host, failClosed)
	return err
}

// resolvePublicHost is the shared classifier. It returns the DNS-resolved public
// addresses of host (nil for a literal or encoded-literal IP, which needs no
// pin: git connects to the address named in the URL and never resolves a name),
// alongside the verdict. The only difference between the fail-open and
// fail-closed modes is how a HostResolver error is handled: allowed through
// (nil, nil) when fail-open, blocked when fail-closed.
func resolvePublicHost(host string, failClosed bool) ([]net.IP, error) {
	lower := strings.ToLower(strings.TrimSpace(host))
	if lower == "" {
		return nil, ErrEmptyHost
	}
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return nil, blockedHostErr(host, "loopback host")
	}
	if ip := net.ParseIP(host); ip != nil {
		if IsInternalIP(ip) {
			return nil, blockedHostErr(host, "internal IP address")
		}
		return nil, nil
	}
	if ip := ParseLooseIPv4(host); ip != nil {
		// Encoded numeric literal (hex, octal, or dotless integer) that
		// net.ParseIP rejects but git's C resolver (getaddrinfo) still decodes to
		// a real address — 0x7f000001, 2130706433, and 0177.0.0.1 all reach
		// 127.0.0.1, and 0xA9FEA9FE reaches the 169.254.169.254 metadata
		// endpoint. Classify the decoded destination so these forms cannot slip
		// an internal target past the fence on a resolver that errors for them.
		if IsInternalIP(ip) {
			return nil, blockedHostErr(host, "internal IP address")
		}
		return nil, nil
	}
	ips, err := HostResolver(host)
	if err != nil {
		if failClosed {
			return nil, blockedHostErr(host, "host resolution failed")
		}
		return nil, nil
	}
	public := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		if IsInternalIP(ip) {
			return nil, blockedHostErr(host, "host resolves to an internal IP address")
		}
		public = append(public, ip)
	}
	return public, nil
}

// internalCIDRv4 are non-public IPv4 ranges Go's net.IP classifiers do NOT cover
// but that a server-side fetch must never target:
//   - 100.64.0.0/10  RFC 6598 shared address space — the CGNAT range used by
//     overlay networks such as Tailscale (net.IP.IsPrivate does not include it,
//     so it is the load-bearing addition);
//   - 0.0.0.0/8      "this host" — 0.x.x.x can route to the local host on Linux;
//   - 192.0.0.0/24   IETF protocol assignments;
//   - 198.18.0.0/15  benchmarking.
//
// A v4-mapped IPv6 (::ffff:a.b.c.d) is unwrapped by To4() so these also fence the
// mapped form.
var internalCIDRv4 = mustCIDRs(
	"100.64.0.0/10",
	"0.0.0.0/8",
	"192.0.0.0/24",
	"198.18.0.0/15",
)

func mustCIDRs(cidrs ...string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("ssrf: invalid internal CIDR constant " + c) // constant list; a bad entry is a build-time bug
		}
		nets = append(nets, n)
	}
	return nets
}

// IsInternalIP reports whether ip is one an internet-facing fetch must never
// target. IsPrivate covers RFC1918 and IPv6 unique-local (fc00::/7); link-local
// covers 169.254.0.0/16 (including the 169.254.169.254 metadata endpoint) and
// fe80::/10; the unspecified address (0.0.0.0, ::) is also internal; and
// internalCIDRv4 adds the ranges Go's classifiers omit (100.64.0.0/10 CGNAT,
// 0.0.0.0/8, 192.0.0.0/24, 198.18.0.0/15).
func IsInternalIP(ip net.IP) bool {
	if ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsUnspecified() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		for _, n := range internalCIDRv4 {
			if n.Contains(v4) {
				return true
			}
		}
		return false
	}
	// A non-v4-mapped IPv6 address may still carry an embedded IPv4 destination
	// the host's transition machinery ultimately routes to. To4() above unwraps
	// only ::ffff:a.b.c.d, so decode the NAT64 (64:ff9b::/96), 6to4 (2002::/16),
	// IPv4-translated (::ffff:0:0:0/96), and deprecated IPv4-compatible (::/96)
	// forms and re-classify the embedded address — otherwise a git URL naming
	// e.g. 64:ff9b::a00:1, 2002:0a00:0001::, or ::ffff:0:10.0.0.1 reaches an
	// RFC1918/internal IPv4 after the fence classifies it public. The same
	// prefixes wrapping a public IPv4 (NAT64/6to4 legitimately do) stay allowed,
	// because the re-classification runs on the decoded address.
	if embedded := embeddedTransitionIPv4(ip); embedded != nil {
		return IsInternalIP(embedded)
	}
	return false
}

// embeddedTransitionIPv4 returns the IPv4 address an IPv6 transition literal
// embeds, or nil when ip is not one of the decoded forms. It recognizes the
// RFC 6052 well-known NAT64 prefix 64:ff9b::/96, the IPv4-translated
// ::ffff:0:0:0/96 form, and the deprecated IPv4-compatible ::/96 form (all with
// the embedded IPv4 in the low 32 bits), plus the 6to4 prefix 2002::/16
// (embedded IPv4 in bits 16..48). The v4-mapped ::ffff:a.b.c.d form is
// intentionally NOT handled here — IsInternalIP's To4() already unwraps it — so
// a v4 or v4-mapped input returns nil and the classifier never recurses on it.
// Teredo and ORCHID are out of scope: they do not encode a routable embedded
// IPv4 destination the fence must re-classify.
func embeddedTransitionIPv4(ip net.IP) net.IP {
	v6 := ip.To16()
	if v6 == nil || ip.To4() != nil {
		return nil
	}
	switch {
	case isNAT64WellKnown(v6):
		return net.IPv4(v6[12], v6[13], v6[14], v6[15])
	case v6[0] == 0x20 && v6[1] == 0x02: // 6to4 2002::/16
		return net.IPv4(v6[2], v6[3], v6[4], v6[5])
	case isIPv4Translated(v6):
		return net.IPv4(v6[12], v6[13], v6[14], v6[15])
	case isIPv4Compatible(v6):
		return net.IPv4(v6[12], v6[13], v6[14], v6[15])
	}
	return nil
}

// isNAT64WellKnown reports whether the 16-byte IPv6 v6 falls in the RFC 6052
// well-known NAT64 prefix 64:ff9b::/96, whose low 32 bits carry the embedded
// IPv4.
func isNAT64WellKnown(v6 net.IP) bool {
	if v6[0] != 0x00 || v6[1] != 0x64 || v6[2] != 0xff || v6[3] != 0x9b {
		return false
	}
	for _, b := range v6[4:12] {
		if b != 0 {
			return false
		}
	}
	return true
}

// isIPv4Translated reports whether the 16-byte IPv6 v6 is an IPv4-translated
// address (::ffff:0:0:0/96, RFC 6052 §2.1), whose low 32 bits carry the embedded
// IPv4. The prefix is eight zero bytes, then 0xffff at bytes 8-9 and zero at
// bytes 10-11 — distinct from the v4-mapped ::ffff:a.b.c.d form (0xffff at bytes
// 10-11), which To4() unwraps and IsInternalIP handles before this is reached.
// Without this decode a literal such as ::ffff:0:10.0.0.1 embeds an RFC1918
// destination yet To4() returns nil, so the classifier would treat it as public.
func isIPv4Translated(v6 net.IP) bool {
	for _, b := range v6[0:8] {
		if b != 0 {
			return false
		}
	}
	return v6[8] == 0xff && v6[9] == 0xff && v6[10] == 0 && v6[11] == 0
}

// isIPv4Compatible reports whether the 16-byte IPv6 v6 is a deprecated
// IPv4-compatible address (::/96, RFC 4291 §2.5.5.1). The ::ffff: v4-mapped form
// carries 0xff at bytes 10-11 and is excluded here (To4() handles it); the ::
// and ::1 forms are already classified by IsUnspecified/IsLoopback before this
// is reached, so any surviving match embeds a non-trivial IPv4 to re-classify.
func isIPv4Compatible(v6 net.IP) bool {
	for _, b := range v6[0:12] {
		if b != 0 {
			return false
		}
	}
	return true
}

// ParseLooseIPv4 decodes the legacy inet_aton host forms that net.ParseIP
// rejects but the C resolver (getaddrinfo, which git and libcurl use) still
// accepts: a dotless 32-bit integer, hex (0x…) or octal (leading 0) parts, and
// the short a.b / a.b.c groupings. It returns the decoded IPv4 address, or nil
// when host is not one of those numeric forms (a normal hostname, or a form
// net.ParseIP already handled). Classifying the decoded address lets the fence
// see the destination git will actually connect to rather than trusting
// net.ParseIP to recognize every literal the resolver decodes.
func ParseLooseIPv4(host string) net.IP {
	if host == "" {
		return nil
	}
	parts := strings.Split(host, ".")
	if len(parts) > 4 {
		return nil
	}
	vals := make([]uint64, len(parts))
	for i, p := range parts {
		v, ok := parseInetAtonPart(p)
		if !ok {
			return nil
		}
		vals[i] = v
	}
	// inet_aton spreads the trailing part across the low-order bytes: a.b puts b
	// in the low 24 bits, a.b.c puts c in the low 16, a.b.c.d is one byte each.
	var addr uint64
	switch len(parts) {
	case 1:
		addr = vals[0]
	case 2:
		if vals[0] > 0xFF || vals[1] > 0xFFFFFF {
			return nil
		}
		addr = vals[0]<<24 | vals[1]
	case 3:
		if vals[0] > 0xFF || vals[1] > 0xFF || vals[2] > 0xFFFF {
			return nil
		}
		addr = vals[0]<<24 | vals[1]<<16 | vals[2]
	case 4:
		for _, v := range vals {
			if v > 0xFF {
				return nil
			}
		}
		addr = vals[0]<<24 | vals[1]<<16 | vals[2]<<8 | vals[3]
	}
	if addr > 0xFFFFFFFF {
		return nil
	}
	return net.IPv4(byte(addr>>24), byte(addr>>16), byte(addr>>8), byte(addr))
}

// parseInetAtonPart parses one component of a loose IPv4 literal with C
// inet_aton radix rules: a 0x/0X prefix is hex, a leading 0 is octal, everything
// else is decimal. It rejects an empty or malformed component.
func parseInetAtonPart(p string) (uint64, bool) {
	base := 10
	digits := p
	switch {
	case len(p) >= 2 && (p[0:2] == "0x" || p[0:2] == "0X"):
		base, digits = 16, p[2:]
	case len(p) >= 2 && p[0] == '0':
		base, digits = 8, p[1:]
	}
	if digits == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(digits, base, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// blockedHostErr wraps ErrBlockedHost with the host and the reason it was
// classified internal, so callers can surface a precise cause.
func blockedHostErr(host, why string) error {
	return fmt.Errorf("%w: %q (%s)", ErrBlockedHost, host, why)
}
