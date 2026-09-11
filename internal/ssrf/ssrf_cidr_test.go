package ssrf

import (
	"net"
	"testing"
)

// TestIsInternalIPCoversNonGoClassifierRanges pins the ranges Go's net.IP
// classifiers omit but a server-side fetch must never reach — chiefly
// 100.64.0.0/10 (RFC 6598 CGNAT, used by overlay networks such as Tailscale).
func TestIsInternalIPCoversNonGoClassifierRanges(t *testing.T) {
	internal := []string{
		"100.64.0.1", "100.100.100.100", "100.127.255.254", // 100.64.0.0/10 CGNAT range
		"0.0.0.1", "0.255.255.255", // 0.0.0.0/8 this-host
		"192.0.0.1", "192.0.0.255", // 192.0.0.0/24 IETF protocol assignments
		"198.18.0.1", "198.19.255.254", // 198.18.0.0/15 benchmarking
		"::ffff:100.64.0.1", // v4-mapped CGNAT
	}
	for _, s := range internal {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test IP %q", s)
		}
		if !IsInternalIP(ip) {
			t.Errorf("IsInternalIP(%s) = false, want true (internal)", s)
		}
	}

	public := []string{
		"8.8.8.8", "1.1.1.1", "93.184.216.34", // genuinely public
		"100.63.255.255", "100.128.0.0", // just outside 100.64.0.0/10
		"198.20.0.1", // just outside 198.18.0.0/15
	}
	for _, s := range public {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test IP %q", s)
		}
		if IsInternalIP(ip) {
			t.Errorf("IsInternalIP(%s) = true, want false (public)", s)
		}
	}
}

// TestIsInternalIPCoversIPv6TransitionalForms pins the IPv6 transition prefixes
// that embed a 32-bit IPv4 destination the host's transition machinery routes
// to. Go's To4() unwraps only the v4-mapped ::ffff:a.b.c.d form, so without the
// embedded-IPv4 decode a git URL naming NAT64 (64:ff9b::/96), 6to4 (2002::/16),
// the IPv4-translated (::ffff:0:0:0/96) form, or the deprecated IPv4-compatible
// (::/96) literal of an internal IPv4 slips past the fence as "public".
// Classification re-runs on the embedded address, so the same prefixes wrapping
// a PUBLIC IPv4 stay allowed (NAT64/6to4 legitimately carry public v4).
func TestIsInternalIPCoversIPv6TransitionalForms(t *testing.T) {
	internal := []string{
		"64:ff9b::a00:1",           // NAT64 well-known embedding 10.0.0.1 (RFC1918)
		"64:ff9b::7f00:1",          // NAT64 embedding 127.0.0.1 (loopback)
		"64:ff9b::a9fe:a9fe",       // NAT64 embedding 169.254.169.254 (cloud metadata)
		"64:ff9b::6440:1",          // NAT64 embedding 100.64.0.1 (CGNAT)
		"2002:a00:1::",             // 6to4 embedding 10.0.0.1
		"2002:c0a8:1::1",           // 6to4 embedding 192.168.0.1
		"2002:a9fe:a9fe::",         // 6to4 embedding 169.254.169.254
		"::ffff:0:10.0.0.1",        // IPv4-translated embedding 10.0.0.1 (RFC1918)
		"::ffff:0:169.254.169.254", // IPv4-translated embedding 169.254.169.254 (cloud metadata)
		"::a00:1",                  // IPv4-compatible embedding 10.0.0.1 (deprecated)
		"::7f00:1",                 // IPv4-compatible embedding 127.0.0.1
	}
	for _, s := range internal {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test IP %q", s)
		}
		if !IsInternalIP(ip) {
			t.Errorf("IsInternalIP(%s) = false, want true (embeds an internal IPv4)", s)
		}
	}

	public := []string{
		"64:ff9b::808:808",     // NAT64 embedding 8.8.8.8 (legitimate public v4-over-NAT64)
		"64:ff9b::5db8:d822",   // NAT64 embedding 93.184.216.34
		"2002:808:808::",       // 6to4 embedding 8.8.8.8
		"::ffff:0:8.8.8.8",     // IPv4-translated embedding 8.8.8.8 (legitimate public)
		"::808:808",            // IPv4-compatible embedding 8.8.8.8
		"2001:4860:4860::8888", // ordinary global-unicast IPv6 (no embedded internal v4)
	}
	for _, s := range public {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test IP %q", s)
		}
		if IsInternalIP(ip) {
			t.Errorf("IsInternalIP(%s) = true, want false (embeds a public IPv4)", s)
		}
	}
}
