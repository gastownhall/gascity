package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCompletionCommandBash(t *testing.T) {
	configureIsolatedRuntimeEnv(t)

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	if code := run([]string{"completion", "bash"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run([completion bash]) = %d, want 0; stderr=%q", code, stderr.String())
	}

	script := stdout.String()
	if !strings.Contains(script, "# bash completion V2 for gc") {
		t.Fatalf("completion output missing bash header:\n%s", script)
	}
	if !strings.Contains(script, "__start_gc()") {
		t.Fatalf("completion output missing gc completion entrypoint:\n%s", script)
	}
}
