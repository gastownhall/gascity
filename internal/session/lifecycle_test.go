package session

import "testing"

func TestRuntimeEnvWithAliasOmitsEmptyAlias(t *testing.T) {
	env := RuntimeEnvWithAlias("sid", "sname", "", DefaultGeneration, DefaultContinuationEpoch, "tok")
	if _, ok := env["GC_ALIAS"]; ok {
		t.Fatalf("GC_ALIAS should be absent when alias is empty; got %q", env["GC_ALIAS"])
	}
}

func TestRuntimeEnvWithAliasSetsNonEmptyAlias(t *testing.T) {
	env := RuntimeEnvWithAlias("sid", "sname", "mayor", DefaultGeneration, DefaultContinuationEpoch, "tok")
	if got := env["GC_ALIAS"]; got != "mayor" {
		t.Fatalf("GC_ALIAS = %q, want %q", got, "mayor")
	}
}
