package dolt_test

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Prevent dolt's event-flush goroutine from blocking subprocess exit
	// for up to 10 minutes. filteredEnv() passes through env vars not in
	// its blocklist, so this propagates to all subprocess dolt invocations.
	if err := os.Setenv("DOLT_DISABLE_EVENT_FLUSH", "true"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// TestDoltEventFlushDisabledInTestProcess pins the TestMain contract:
// DOLT_DISABLE_EVENT_FLUSH must be true in the test process so dolt
// subprocesses do not block on event-flush goroutine exit.
func TestDoltEventFlushDisabledInTestProcess(t *testing.T) {
	if os.Getenv("DOLT_DISABLE_EVENT_FLUSH") != "true" {
		t.Fatal("DOLT_DISABLE_EVENT_FLUSH not set to true by TestMain — dolt subprocesses will hang for up to 10 min on event flush")
	}
}
