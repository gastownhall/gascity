//go:build windows

package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestGeneratedRunShellCommandIncludesStderrOnWindows(t *testing.T) {
	check := (&config.Agent{Name: "worker"}).EffectivePoolDemandQuery()
	target := generatedTarget(t, check)
	installFakeBdOnPath(t, "", map[string]nativeFakeBdReply{
		"ready --metadata-field gc.routed_to=" + target: {Exit: 7, Stderr: "store exploded"},
		"": {Stdout: "[]"},
	})

	out, err := shellScaleCheck(check, t.TempDir(), nil)
	if err == nil {
		t.Fatalf("shellScaleCheck = %q, nil; want failure with stderr", out)
	}
	if !strings.Contains(err.Error(), "store exploded") {
		t.Fatalf("shellScaleCheck error = %v; want generated stderr", err)
	}
}
