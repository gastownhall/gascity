//go:build !linux

package main

import (
	"bytes"
	"testing"
)

func TestFSPressureNoopOnNonLinux(t *testing.T) {
	var stderr bytes.Buffer
	cr := &CityRuntime{stderr: &stderr}
	if cr.shouldSkipTickForFSPressure(nil, "patrol") {
		t.Fatal("expected non-Linux FS pressure gate to be a no-op")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want no output", stderr.String())
	}
	avg60, err := readFSPressureAvg60(fsPressurePath)
	if err != nil {
		t.Fatalf("readFSPressureAvg60 returned error on non-Linux: %v", err)
	}
	if avg60 != 0 {
		t.Fatalf("avg60 = %v, want 0 on non-Linux", avg60)
	}
}
