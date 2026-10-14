//go:build ignore

package main

import (
	"os"

	"github.com/gastownhall/gascity/internal/testpolicy/shardbudget"
)

func main() {
	os.Exit(shardbudget.Run(os.Args[1:], os.Stdout, os.Stderr))
}
