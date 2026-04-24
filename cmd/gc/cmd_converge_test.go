package main

import (
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/convergence"
)

func TestConvergeCreateGateTimeoutDefaultMatchesSharedDefault(t *testing.T) {
	cmd := newConvergeCreateCmd(io.Discard, io.Discard)
	flag := cmd.Flags().Lookup("gate-timeout")
	if flag == nil {
		t.Fatal("gate-timeout flag not found")
	}

	want := convergence.DefaultGateTimeout.String()
	if flag.DefValue != want {
		t.Fatalf("gate-timeout default = %q, want %q", flag.DefValue, want)
	}
	if got := flag.Value.String(); got != want {
		t.Fatalf("gate-timeout bound value = %q, want %q", got, want)
	}
}
