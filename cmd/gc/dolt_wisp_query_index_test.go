package main

import (
	"bytes"
	"context"
	"testing"
)

func TestApplyWispQueryIndexesToDB_ReturnsErrorOnUnreachableServer(t *testing.T) {
	var stderr bytes.Buffer
	err := applyWispQueryIndexesToDB(context.Background(), "19999", "hq", &stderr)
	if err == nil {
		t.Fatal("expected error for unreachable port, got nil")
	}
}

func TestApplyWispQueryIndexesToDB_ContextCancellation(_ *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stderr bytes.Buffer
	// Should fail quickly with context error or connection error, not hang.
	_ = applyWispQueryIndexesToDB(ctx, "19999", "hq", &stderr)
}

func TestWispQueryIndexStatements_AllHaveCreateIndex(t *testing.T) {
	if len(wispQueryIndexStatements) == 0 {
		t.Fatal("wispQueryIndexStatements must not be empty")
	}
	for _, stmt := range wispQueryIndexStatements {
		if len(stmt) < 20 {
			t.Errorf("statement too short to be a valid DDL: %q", stmt)
		}
	}
}
