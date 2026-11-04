//go:build ignore

package main

import (
	"os"

	"github.com/gastownhall/gascity/internal/testpolicy/bepsummary"
)

func main() {
	os.Exit(bepsummary.Run(os.Args[1:], os.Stdout, os.Stderr))
}
