//go:build windows

package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/genshell"
	"github.com/gastownhall/gascity/internal/shellquote"
)

func TestGeneratedProgramRejectsSameScriptDifferentProgramOnWindows(t *testing.T) {
	agent := &config.Agent{Name: "worker"}
	command := agent.EffectiveWorkQuery()
	prog, err := genshell.Parse(command)
	if err != nil {
		t.Fatalf("generated work query does not parse: %v", err)
	}
	if generatedProgram(command) == nil {
		t.Fatal("the exact generated command was not recognized")
	}

	for name, candidate := range map[string]string{
		"different target":      shellquote.Join([]string{"sh", "-c", prog.Script, "--", "other-worker"}),
		"different script name": shellquote.Join([]string{"sh", "-c", prog.Script, "different-name", prog.Args[0]}),
		"prefix assignment":     "GC_DOLT_PORT=3307 " + command,
	} {
		if generatedProgram(candidate) != nil {
			t.Fatalf("%s: command was recognized from script text alone: %q", name, candidate)
		}
	}
}
