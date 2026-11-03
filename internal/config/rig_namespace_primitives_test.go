package config

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRigRouteQualified(t *testing.T) {
	if !RigRouteQualified("  beta/x\t") {
		t.Error("RigRouteQualified does not trim surrounding space")
	}
	for v, want := range map[string]bool{
		"beta/crew.worker":   true,
		"beta--crew__worker": true,
		"crew.worker":        false,
		"":                   false,
		"/x":                 false,
		"x/":                 false,
		"a//b":               false,
		"--x":                false,
		"beta/":              false,
	} {
		if got := RigRouteQualified(v); got != want {
			t.Errorf("RigRouteQualified(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestPoolSessionIDShapes(t *testing.T) {
	token := strings.Repeat("0a", 16)
	for id, want := range map[string]bool{
		"gcx-session-" + token:                  true,
		"g_c.x-y-session-" + token:              true,
		"gcx-17":                                false,
		"gcx-session-" + token[:30]:             false,
		"gcx-session-" + strings.ToUpper(token): false,
		"crew.worker-gcx-session-" + token:      true,
	} {
		if got := IsPoolSessionBeadID(id); got != want {
			t.Errorf("IsPoolSessionBeadID(%q) = %v, want %v", id, got, want)
		}
	}
	if !EmbedsPoolSessionID(" crew.worker-gcx-session-"+token+" ") || EmbedsPoolSessionID("beta/crew.worker") {
		t.Error("EmbedsPoolSessionID misreads a pool session name or a rig identity")
	}
	if IsPoolSessionBeadID(strings.Repeat("a", MaxPoolSessionIdentityLen) + "-session-" + token) {
		t.Error("an over-long id passed as a pool session id")
	}
}

func TestErrRigNamespaceWraps(t *testing.T) {
	if !errors.Is(fmt.Errorf("refusing: %w", ErrRigNamespace), ErrRigNamespace) {
		t.Fatal("a wrapped refusal does not match ErrRigNamespace")
	}
}
