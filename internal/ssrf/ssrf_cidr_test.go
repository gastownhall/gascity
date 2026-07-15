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
