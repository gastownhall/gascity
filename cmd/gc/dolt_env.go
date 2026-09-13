package main

import (
	"os"
	"strings"
)

func gcDoltSkip() bool {
	return strings.TrimSpace(os.Getenv("GC_DOLT")) == "skip"
}
