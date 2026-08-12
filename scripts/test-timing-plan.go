//go:build ignore

package main

import (
	"os"

	"github.com/gastownhall/gascity/internal/testpolicy/timingplancli"
)

func main() {
	os.Exit(timingplancli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
