package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestReconcileProviderOwnedScopeIdentityProxiedUsesOnePrivateSession: with a
// live record, the reconcile's L3 is exactly one TCP session to the proxy's
// loopback data port, through the real L3 opener. The handshake failing here
// proves the dial reached that port and no redial ladder followed, and a
// session that cannot open leaves every layer untouched.
func TestReconcileProviderOwnedScopeIdentityProxiedUsesOnePrivateSession(t *testing.T) {
	clearInheritedBeadsEnv(t)
	clearProviderOwnedProxyEnv(t)
	scope := t.TempDir()
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := []byte(proxiedIdentityMetadata(providerOwnedIdentityP))
	if err := os.WriteFile(scopeMetadataJSONPath(scope), metadata, 0o600); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", net.JoinHostPort(proxyendpoint.Host, "0"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var accepted atomic.Int64
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			accepted.Add(1)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	stubLiveProviderOwnedProxyOnPort(t, listener.Addr().(*net.TCPAddr).Port)

	err = reconcileProviderOwnedScopeIdentity(context.Background(), t.TempDir(), scope)
	if err == nil || !strings.Contains(err.Error(), "open bd proxy session") {
		t.Fatalf("reconcile against a proxy that closes at once = %v, want the session-open failure", err)
	}
	if got := accepted.Load(); got != 1 {
		t.Fatalf("the reconcile dialed the proxy %d time(s), want exactly 1: the session is private and single-shot, never database/sql's redial ladder", got)
	}
	if _, ok, readErr := contract.ReadProjectIdentity(fsys.OSFS{}, scope); readErr != nil || ok {
		t.Fatalf("identity.toml after a failed session = (%t, %v), want absent: no layer is written when L3 cannot open", ok, readErr)
	}
	after, err := os.ReadFile(scopeMetadataJSONPath(scope))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, metadata) {
		t.Fatalf("metadata.json after a failed session = %s, want it unchanged: %s", after, metadata)
	}
}
