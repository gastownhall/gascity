package main

import "testing"

func TestCurrentSessionRuntimeTargetUsesAlias(t *testing.T) {
	t.Setenv("GC_CITY", "/tmp/city")
	t.Setenv("GC_ALIAS", "mayor")
	t.Setenv("GC_SESSION_ID", "gc-42")
	t.Setenv("GC_SESSION_NAME", "s-gc-42")

	got, err := currentSessionRuntimeTarget()
	if err != nil {
		t.Fatalf("currentSessionRuntimeTarget(): %v", err)
	}
	if got.cityPath != "/tmp/city" {
		t.Fatalf("cityPath = %q, want /tmp/city", got.cityPath)
	}
	if got.display != "mayor" {
		t.Fatalf("display = %q, want mayor", got.display)
	}
	if got.sessionName != "s-gc-42" {
		t.Fatalf("sessionName = %q, want s-gc-42", got.sessionName)
	}
}

func TestEventActorPrefersAliasThenSessionID(t *testing.T) {
	t.Setenv("GC_ALIAS", "mayor")
	t.Setenv("GC_SESSION_ID", "gc-42")
	if got := eventActor(); got != "mayor" {
		t.Fatalf("eventActor() = %q, want mayor", got)
	}

	t.Setenv("GC_ALIAS", "")
	if got := eventActor(); got != "gc-42" {
		t.Fatalf("eventActor() without alias = %q, want gc-42", got)
	}
}
