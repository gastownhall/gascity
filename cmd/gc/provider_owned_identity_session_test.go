package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
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
	writeReadyTopologyScope(t, scope, readyTopologyScope{
		beadsConfig: bdTemplateBeadsConfig,
		metadata:    proxiedIdentityMetadata(providerOwnedIdentityP),
	})
	metadata := mustReadFile(t, scopeMetadataJSONPath(scope))

	// The listener binds 127.0.0.1, which is proxyendpoint.Host: the loopback
	// address the session dials.
	port, accepts := acceptCountingListener(t)
	stubLiveProviderOwnedProxyOnPort(t, port)

	err := reconcileProviderOwnedScopeIdentity(context.Background(), scope, scope)
	if err == nil || !strings.Contains(err.Error(), "open bd proxy session") {
		t.Fatalf("reconcile against a proxy that closes at once = %v, want the session-open failure", err)
	}
	if got := accepts(); got != 1 {
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
