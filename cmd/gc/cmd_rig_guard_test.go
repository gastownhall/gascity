package main

import (
	"bytes"
	"testing"
)

func TestRigAgentGuard(t *testing.T) {
	t.Run("rig agent is blocked from gc stop", func(t *testing.T) {
		t.Setenv("GC_AGENT", "gascity/polecat")
		var stdout, stderr bytes.Buffer
		code := run([]string{"stop"}, &stdout, &stderr)
		if code == 0 {
			t.Fatal("expected non-zero exit for rig agent running gc stop")
		}
		if !bytes.Contains(stderr.Bytes(), []byte("not permitted from rig agents")) {
			t.Errorf("expected rig-agent error in stderr, got: %s", stderr.String())
		}
	})

	t.Run("rig agent is blocked from gc start", func(t *testing.T) {
		t.Setenv("GC_AGENT", "gascity/polecat")
		var stdout, stderr bytes.Buffer
		code := run([]string{"start"}, &stdout, &stderr)
		if code == 0 {
			t.Fatal("expected non-zero exit for rig agent running gc start")
		}
		if !bytes.Contains(stderr.Bytes(), []byte("not permitted from rig agents")) {
			t.Errorf("expected rig-agent error in stderr, got: %s", stderr.String())
		}
	})

	t.Run("rig agent is blocked from gc suspend", func(t *testing.T) {
		t.Setenv("GC_AGENT", "gascity/polecat")
		var stdout, stderr bytes.Buffer
		code := run([]string{"suspend"}, &stdout, &stderr)
		if code == 0 {
			t.Fatal("expected non-zero exit for rig agent running gc suspend")
		}
		if !bytes.Contains(stderr.Bytes(), []byte("not permitted from rig agents")) {
			t.Errorf("expected rig-agent error in stderr, got: %s", stderr.String())
		}
	})

	t.Run("city agent is not blocked from gc stop", func(t *testing.T) {
		t.Setenv("GC_AGENT", "mayor")
		var stdout, stderr bytes.Buffer
		run([]string{"stop"}, &stdout, &stderr) // may fail (no city), but not for RBAC
		if bytes.Contains(stderr.Bytes(), []byte("not permitted from rig agents")) {
			t.Errorf("city agent should not be blocked: %s", stderr.String())
		}
	})

	t.Run("empty GC_AGENT is not blocked", func(t *testing.T) {
		t.Setenv("GC_AGENT", "")
		var stdout, stderr bytes.Buffer
		run([]string{"stop"}, &stdout, &stderr)
		if bytes.Contains(stderr.Bytes(), []byte("not permitted from rig agents")) {
			t.Errorf("empty GC_AGENT should not be blocked: %s", stderr.String())
		}
	})

	t.Run("sling is allowed for rig agents", func(t *testing.T) {
		t.Setenv("GC_AGENT", "gascity/polecat")
		var stdout, stderr bytes.Buffer
		run([]string{"sling", "--help"}, &stdout, &stderr)
		if bytes.Contains(stderr.Bytes(), []byte("not permitted from rig agents")) {
			t.Errorf("sling should be allowed for rig agents: %s", stderr.String())
		}
	})
}
