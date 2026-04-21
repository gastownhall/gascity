package main

import (
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/fsys"
)

func ensureInitArtifacts(cityPath string, stderr io.Writer, commandName string) {
	if commandName == "" {
		commandName = "gc start"
	}
	if code := installClaudeHooks(fsys.OSFS{}, cityPath, stderr); code != 0 {
		fmt.Fprintf(stderr, "%s: installing claude hooks: exit %d\n", commandName, code) //nolint:errcheck // best-effort stderr
	}
}
