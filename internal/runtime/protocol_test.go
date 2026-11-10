package runtime_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestSharesHostFSNeedsLocalityAndToken pins the one predicate that reads
// fs.host: it narrows locality=local, so it holds only when the handshake
// declares both. fs.host without locality=local reads as undeclared, and
// locality=local alone (a container on the controller's host) promises no
// shared filesystem view or user.
func TestSharesHostFSNeedsLocalityAndToken(t *testing.T) {
	cases := []struct {
		name string
		info runtime.ProtocolInfo
		want bool
	}{
		{"local and fs.host", runtime.ProtocolInfo{Locality: runtime.ProtocolLocalityLocal, Capabilities: []string{runtime.ProtocolCapabilityFSHost}}, true},
		{"local without fs.host", runtime.ProtocolInfo{Locality: runtime.ProtocolLocalityLocal, Capabilities: []string{runtime.ProtocolCapabilityObserveV1}}, false},
		{"fs.host without locality", runtime.ProtocolInfo{Capabilities: []string{runtime.ProtocolCapabilityFSHost}}, false},
		{"fs.host with a remote locality", runtime.ProtocolInfo{Locality: "remote", Capabilities: []string{runtime.ProtocolCapabilityFSHost}}, false},
		{"zero handshake", runtime.ProtocolInfo{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.info.SharesHostFS(); got != tc.want {
				t.Errorf("SharesHostFS() = %v, want %v", got, tc.want)
			}
		})
	}
	if runtime.ProtocolCapabilityFSHost != "fs.host" {
		t.Errorf("ProtocolCapabilityFSHost = %q, want the wire token %q", runtime.ProtocolCapabilityFSHost, "fs.host")
	}
}
