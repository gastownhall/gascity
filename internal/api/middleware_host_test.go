package api

import "testing"

// TestIsAllowedSupervisorHost pins the full Host-header allowlist corpus for
// the DNS-rebinding defense (#2723). The attack variants are ported from the
// adversarially reviewed dashboard host-allowlist branch (abe825284): a
// browser tricked into resolving an attacker name to a loopback bind carries
// the attacker's Host header, so every non-loopback, non-configured identity
// must be rejected. Shorthand loopback spellings a browser could normalize
// (decimal/octal/short IPs) are rejected rather than allowed — the safe
// direction.
func TestIsAllowedSupervisorHost(t *testing.T) {
	cases := []struct {
		name  string
		host  string
		extra []string
		want  bool
	}{
		// Genuine loopback identities — always allowed.
		{"localhost", "localhost", nil, true},
		{"localhost with port", "localhost:8080", nil, true},
		{"uppercase localhost", "LOCALHOST:8080", nil, true},
		{"ipv4 loopback", "127.0.0.1", nil, true},
		{"ipv4 loopback with port", "127.0.0.1:8080", nil, true},
		{"ipv4 loopback range", "127.0.0.2:8080", nil, true},
		{"ipv6 loopback", "::1", nil, true},
		{"ipv6 loopback bracketed", "[::1]", nil, true},
		{"ipv6 loopback with port", "[::1]:8080", nil, true},
		{"ipv4-mapped ipv6 loopback", "[::ffff:127.0.0.1]", nil, true},
		// The next two pin incidental parser tolerance (SplitHostPort accepts
		// an empty port; ParseIP accepts unbracketed expanded IPv6), not
		// contract — tightening them to rejection is an acceptable change.
		{"ipv4 loopback empty port", "127.0.0.1:", nil, true},
		{"fully expanded ipv6 loopback", "0:0:0:0:0:0:0:1", nil, true},

		// Attack forms — all rejected.
		{"empty host", "", nil, false},
		{"public host", "evil.example", nil, false},
		{"public host with port", "evil.example:8080", nil, false},
		{"localhost-suffix attack", "localhost.evil.example", nil, false},
		{"private ip", "192.168.1.20:8080", nil, false},
		{"userinfo", "evil@127.0.0.1", nil, false},
		{"userinfo with port", "evil@127.0.0.1:8080", nil, false},
		{"space injection", "127.0.0.1 evil.example", nil, false},
		{"tab injection", "127.0.0.1\tevil", nil, false},
		{"null byte injection", "localhost\x00.evil", nil, false},
		{"percent-encoded null", "127.0.0.1%00", nil, false},
		{"decimal ip", "2130706433", nil, false},
		{"short ip", "127.1", nil, false},
		{"octal ip", "0177.0.0.1", nil, false},
		{"trailing dot localhost", "localhost.", nil, false},
		{"trailing dot ipv4", "127.0.0.1.", nil, false},
		{"unspecified ipv4", "0.0.0.0", nil, false},
		{"unspecified ipv4 with port", "0.0.0.0:8080", nil, false},
		{"bare port", ":8080", nil, false},
		{"ipv6 zone id", "[::1%eth0]", nil, false},
		{"hex-encoded loopback", "0x7f000001", nil, false},
		{"ipv4-mapped ipv6 non-loopback", "[::ffff:169.254.169.254]", nil, false},
		{"ipv4-compatible ipv6 loopback embed", "::127.0.0.1", nil, false},
		{"homoglyph localhost", "lοcalhost", nil, false},

		// Operator allowlist — hostname-only match, case-insensitive,
		// ports ignored on both sides.
		{"configured host", "dash.internal:8080", []string{"dash.internal"}, true},
		{"configured host case-insensitive", "Dash.Internal:8080", []string{"dash.internal"}, true},
		{"configured host with port in allowlist", "dash.internal:8080", []string{"dash.internal:9999"}, true},
		{"unconfigured host rejected", "other.internal:8080", []string{"dash.internal"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAllowedSupervisorHost(tc.host, tc.extra); got != tc.want {
				t.Fatalf("isAllowedSupervisorHost(%q, %v) = %v, want %v", tc.host, tc.extra, got, tc.want)
			}
		})
	}
}
