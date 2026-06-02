package coordstore_test

import (
	"testing"
	"time"
)

func TestSoakConfigFromEnvParsesSeparateChaosDuration(t *testing.T) {
	t.Setenv("COORDSTORE_SOAK_DURATION", "6h")
	t.Setenv("COORDSTORE_CHAOS_DURATION", "1h")

	cfg := soakConfigFromEnv(t, 4*time.Hour)
	if cfg.SoakDuration != 6*time.Hour {
		t.Fatalf("SoakDuration = %s, want 6h", cfg.SoakDuration)
	}
	if cfg.ChaosDuration != time.Hour {
		t.Fatalf("ChaosDuration = %s, want 1h", cfg.ChaosDuration)
	}
}
