package main

import (
	"os"
	"strings"
)

// gcDoltSkip checks if the GC_DOLT environment variable is set to 'skip'.
func gcDoltSkip() bool {
	return strings.TrimSpace(os.Getenv("GC_DOLT")) == "skip"
}
