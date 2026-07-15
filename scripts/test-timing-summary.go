//go:build ignore

package main

import (
	"os"

	"github.com/gastownhall/gascity/internal/testpolicy/timingsummary"
)

func main() {
	os.Exit(timingsummary.Run(os.Args[1:], os.Stdout, os.Stderr))
}
