//go:build ignore

package main

import (
	"os"

	"github.com/gastownhall/gascity/internal/testpolicy/censusgrowthcli"
)

func main() {
	os.Exit(censusgrowthcli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
