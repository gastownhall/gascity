package envrequired

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" {
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestRunsWhenAuthEnvSurvives(t *testing.T) {}
