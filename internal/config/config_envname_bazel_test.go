package config

import "testing"

func TestEnvVarNameBazelPilot(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"GC_TOKEN", true}, {"_private2", true}, {"1INVALID", false}, {"", false},
	} {
		if got := envVarName.MatchString(tc.name); got != tc.want {
			t.Errorf("envVarName.MatchString(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
