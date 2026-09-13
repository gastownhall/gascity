package ssrf

import (
	"errors"
	"net"
	"strings"
	"testing"
)

// stubResolver swaps the DNS seam for the test and returns a restore func. Hosts
// absent from table resolve with an error (no address), mirroring the pack
// fence's stub so the two fences share a test discipline.
func stubResolver(t *testing.T, table map[string][]net.IP) func() {
	t.Helper()
	orig := HostResolver
	HostResolver = func(host string) ([]net.IP, error) {
		if ips, ok := table[strings.ToLower(host)]; ok {
			return ips, nil
		}
		return nil, errors.New("no such host")
	}
	return func() { HostResolver = orig }
}

func TestEnsurePublicHost_BlocksInternalLiterals(t *testing.T) {
	// No DNS is consulted for a literal, so no stub is needed.
	for _, host := range []string{
		"169.254.169.254", // link-local / cloud metadata
		"127.0.0.1",       // loopback
		"10.0.0.5",        // RFC1918
		"192.168.1.1",     // RFC1918
		"172.16.0.1",      // RFC1918
		"0.0.0.0",         // unspecified
		"::1",             // IPv6 loopback
		"fe80::1",         // IPv6 link-local
		"fc00::1",         // IPv6 unique-local
	} {
		if err := EnsurePublicHost(host); !errors.Is(err, ErrBlockedHost) {
			t.Errorf("EnsurePublicHost(%q) = %v, want ErrBlockedHost", host, err)
		}
	}
}

func TestEnsurePublicHost_BlocksEncodedInternalLiterals(t *testing.T) {
	// inet_aton literals net.ParseIP rejects but git's C resolver decodes to an
	// internal address must be blocked via ParseLooseIPv4.
	for _, host := range []string{
		"0xA9FEA9FE", // hex -> 169.254.169.254 (metadata)
		"0xa9fea9fe", // lowercase hex -> 169.254.169.254
		"2852039166", // dotless decimal -> 169.254.169.254
		"0x7f000001", // hex -> 127.0.0.1
		"2130706433", // dotless decimal -> 127.0.0.1
		"0177.0.0.1", // octal octet -> 127.0.0.1
		"3232235521", // dotless decimal -> 192.168.0.1
	} {
		if err := EnsurePublicHost(host); !errors.Is(err, ErrBlockedHost) {
			t.Errorf("EnsurePublicHost(%q) = %v, want ErrBlockedHost", host, err)
		}
	}
}

func TestEnsurePublicHost_BlocksLoopbackHostnames(t *testing.T) {
	for _, host := range []string{"localhost", "LOCALHOST", "api.localhost"} {
		if err := EnsurePublicHost(host); !errors.Is(err, ErrBlockedHost) {
			t.Errorf("EnsurePublicHost(%q) = %v, want ErrBlockedHost", host, err)
		}
	}
}

func TestEnsurePublicHost_EmptyHost(t *testing.T) {
	for _, host := range []string{"", "   "} {
		if err := EnsurePublicHost(host); !errors.Is(err, ErrEmptyHost) {
			t.Errorf("EnsurePublicHost(%q) = %v, want ErrEmptyHost", host, err)
		}
	}
}

func TestEnsurePublicHost_BlocksHostResolvingToInternal(t *testing.T) {
	restore := stubResolver(t, map[string][]net.IP{
		"evil.example.com": {net.ParseIP("169.254.169.254")},
		"rebind.example":   {net.ParseIP("10.1.2.3")},
	})
	defer restore()

	for _, host := range []string{"evil.example.com", "rebind.example"} {
		if err := EnsurePublicHost(host); !errors.Is(err, ErrBlockedHost) {
			t.Errorf("EnsurePublicHost(%q) = %v, want ErrBlockedHost", host, err)
		}
	}
}

func TestEnsurePublicHost_AllowsPublic(t *testing.T) {
	restore := stubResolver(t, map[string][]net.IP{
		"github.com": {net.ParseIP("140.82.112.3")},
	})
	defer restore()

	for _, host := range []string{
		"github.com", // resolves public
		"8.8.8.8",    // public literal
		"0x08080808", // hex -> 8.8.8.8 (public)
		"134744072",  // dotless decimal -> 8.8.8.8 (public)
	} {
		if err := EnsurePublicHost(host); err != nil {
			t.Errorf("EnsurePublicHost(%q) = %v, want nil", host, err)
		}
	}
}

func TestEnsurePublicHost_ResolutionErrorDoesNotBlock(t *testing.T) {
	// A transient DNS failure must not block: git performs its own resolution and
	// surfaces the failure there. The fence blocks only on a positively-internal
	// address.
	restore := stubResolver(t, nil)
	defer restore()

	if err := EnsurePublicHost("unresolvable.invalid"); err != nil {
		t.Errorf("EnsurePublicHost on resolution error = %v, want nil", err)
	}
}

func TestEnsurePublicHostStrict_ResolutionErrorBlocks(t *testing.T) {
	// The fail-closed variant treats a resolution error as a block (the clone
	// path's fence): an attacker forcing a SERVFAIL must not slip past to win the
	// DNS-rebinding TOCTOU at git's own re-resolution. The fail-open variant on
	// the SAME host allows it — the whole point of the split.
	restore := stubResolver(t, nil)
	defer restore()

	if err := EnsurePublicHostStrict("unresolvable.invalid"); !errors.Is(err, ErrBlockedHost) {
		t.Errorf("EnsurePublicHostStrict on resolution error = %v, want ErrBlockedHost", err)
	}
	if err := EnsurePublicHost("unresolvable.invalid"); err != nil {
		t.Errorf("EnsurePublicHost on resolution error = %v, want nil (fail-open contrast)", err)
	}
}

func TestEnsurePublicHostStrict_AllowsPublicAndBlocksInternal(t *testing.T) {
	// Fail-closed only changes the resolution-error arm: a public host still
	// passes, an internal-resolving host still blocks, an empty host still
	// reports ErrEmptyHost.
	restore := stubResolver(t, map[string][]net.IP{
		"github.com":       {net.ParseIP("140.82.112.3")},
		"evil.example.com": {net.ParseIP("169.254.169.254")},
	})
	defer restore()

	if err := EnsurePublicHostStrict("github.com"); err != nil {
		t.Errorf("EnsurePublicHostStrict(github.com) = %v, want nil", err)
	}
	if err := EnsurePublicHostStrict("evil.example.com"); !errors.Is(err, ErrBlockedHost) {
		t.Errorf("EnsurePublicHostStrict(evil) = %v, want ErrBlockedHost", err)
	}
	if err := EnsurePublicHostStrict("  "); !errors.Is(err, ErrEmptyHost) {
		t.Errorf("EnsurePublicHostStrict(empty) = %v, want ErrEmptyHost", err)
	}
}

func TestParseLooseIPv4_NonNumericIsNil(t *testing.T) {
	for _, host := range []string{"github.com", "example.org", "not.an.ip.addr", ""} {
		if ip := ParseLooseIPv4(host); ip != nil {
			t.Errorf("ParseLooseIPv4(%q) = %v, want nil", host, ip)
		}
	}
}
