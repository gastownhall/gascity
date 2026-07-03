package api

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/importsvc"
)

// packSourceHostResolver resolves a hostname to its IP addresses for the SSRF
// fence. It is a package var so tests can stub DNS without touching the network;
// the default uses the process resolver.
var packSourceHostResolver = func(host string) ([]net.IP, error) {
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

// validateHTTPPackSource is the HTTP-layer SSRF fence for POST /packs. The
// import service shells `git ls-remote <source>` synchronously and documents
// that HTTP callers must validate the source first (importsvc/source.go's
// defaultHeadCommit). A network caller must not be able to (a) point the server
// at arbitrary local filesystem paths or file:// repos, or (b) drive git
// fetches at loopback, private, link-local, or cloud-metadata destinations. The
// CLI is a trusted local caller and keeps its local-path support; this fence is
// only applied on the HTTP path.
//
// Host validation alone is NOT sufficient: a fenced public host can redirect
// the git fetch to an internal target, and git re-resolves the host at fetch
// time (DNS rebinding). The redirect and transport-abuse classes are closed at
// the git subprocess by git.UntrustedRemoteGitConfigArgs (redirects disabled,
// transports constrained) on both the HEAD probe and the packman clone/tags
// fetch. The DNS-rebinding TOCTOU window remains an accepted residual — git
// re-resolves at fetch time and pinning the resolved IP is out of scope — so
// this fence is one layer of defense in depth, not the sole control.
//
// Blocked sources return an ErrInvalidSource so packImportHTTPError maps them to
// 400, and importantly they never reach the packAddImport seam.
func validateHTTPPackSource(source string) error {
	host, local, file := packSourceHost(source)
	switch {
	case file:
		return fmt.Errorf("%w: file:// sources are not permitted over the API", importsvc.ErrInvalidSource)
	case local:
		return fmt.Errorf("%w: local filesystem sources are not permitted over the API; use a remote git URL", importsvc.ErrInvalidSource)
	case host == "":
		return fmt.Errorf("%w: could not determine a host from the pack source", importsvc.ErrInvalidSource)
	}
	return ensurePublicPackSourceHost(host)
}

// packSourceHost classifies an import source and extracts its network host.
// It reports local=true for local filesystem paths and file=true for file://
// sources; for remote git sources it returns the host and local=file=false.
// The remote-source detection mirrors importsvc.isRemoteImportSource.
func packSourceHost(source string) (host string, local, file bool) {
	switch {
	case strings.HasPrefix(source, "file://"):
		return "", false, true
	case strings.HasPrefix(source, "git@"):
		// scp-like syntax: user@host:path — the host ends at the first ':' or '/'.
		rest := strings.TrimPrefix(source, "git@")
		if strings.HasPrefix(rest, "[") {
			// Bracketed IPv6 literal host, e.g. git@[::1]:repo. Take the address
			// between the brackets; scanning for ':' would otherwise cut at the
			// first ':' inside the literal and yield the bogus host "[".
			if end := strings.IndexByte(rest, ']'); end > 1 {
				return rest[1:end], false, false
			}
			return "", false, false
		}
		if i := strings.IndexAny(rest, ":/"); i >= 0 {
			return rest[:i], false, false
		}
		return rest, false, false
	case strings.HasPrefix(source, "ssh://"),
		strings.HasPrefix(source, "https://"),
		strings.HasPrefix(source, "http://"):
		if u, err := url.Parse(source); err == nil {
			return u.Hostname(), false, false
		}
		return "", false, false
	case strings.HasPrefix(source, "github.com/"):
		return "github.com", false, false
	default:
		// Everything else is a local path (//, ~, absolute, or relative), the
		// same set importsvc resolves against the city directory.
		return "", true, false
	}
}

// ensurePublicPackSourceHost rejects a host that names or resolves to an
// internal destination (loopback, private, link-local, unique-local,
// unspecified, or a cloud metadata IP such as 169.254.169.254). Hostnames are
// resolved through packSourceHostResolver; a resolution error is not treated as
// a block, since the subsequent git fetch performs its own resolution and will
// surface the failure — the fence only blocks on a positively-internal address.
func ensurePublicPackSourceHost(host string) error {
	lower := strings.ToLower(strings.TrimSpace(host))
	if lower == "" {
		return fmt.Errorf("%w: could not determine a host from the pack source", importsvc.ErrInvalidSource)
	}
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return blockedPackHostErr(host, "loopback host")
	}
	if ip := net.ParseIP(host); ip != nil {
		if isInternalIP(ip) {
			return blockedPackHostErr(host, "internal IP address")
		}
		return nil
	}
	if ip := parseLooseIPv4(host); ip != nil {
		// Encoded numeric literal (hex, octal, or dotless integer) that net.ParseIP
		// rejects but git's C resolver (getaddrinfo) still decodes to a real
		// address — 0x7f000001, 2130706433, and 0177.0.0.1 all reach 127.0.0.1, and
		// 0xA9FEA9FE reaches the 169.254.169.254 metadata endpoint. Classify the
		// decoded destination so these forms cannot slip an internal target past
		// the fence on a resolver that errors for them.
		if isInternalIP(ip) {
			return blockedPackHostErr(host, "internal IP address")
		}
		return nil
	}
	ips, err := packSourceHostResolver(host)
	if err != nil {
		return nil
	}
	for _, ip := range ips {
		if isInternalIP(ip) {
			return blockedPackHostErr(host, "host resolves to an internal IP address")
		}
	}
	return nil
}

// isInternalIP reports whether ip is one an internet-facing pack source must
// never be. IsPrivate covers RFC1918 and IPv6 unique-local (fc00::/7);
// link-local covers 169.254.0.0/16 (including the 169.254.169.254 metadata
// endpoint) and fe80::/10.
func isInternalIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsUnspecified()
}

// parseLooseIPv4 decodes the legacy inet_aton host forms that net.ParseIP
// rejects but the C resolver (getaddrinfo, which git and libcurl use) still
// accepts: a dotless 32-bit integer, hex (0x…) or octal (leading 0) parts, and
// the short a.b / a.b.c groupings. It returns the decoded IPv4 address, or nil
// when host is not one of those numeric forms (a normal hostname, or a form
// net.ParseIP already handled). Classifying the decoded address lets the SSRF
// fence see the destination git will actually connect to rather than trusting
// net.ParseIP to recognize every literal the resolver decodes.
func parseLooseIPv4(host string) net.IP {
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

func blockedPackHostErr(host, why string) error {
	return fmt.Errorf("%w: pack source host %q is blocked (%s)", importsvc.ErrInvalidSource, host, why)
}
