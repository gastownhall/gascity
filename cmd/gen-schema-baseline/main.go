// Command gen-schema-baseline seeds and extends the committed schema
// contract baseline.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "gen-schema-baseline:", err)
		os.Exit(1)
	}
}

func run(_ []string, _ func(string) string, _ io.Writer) error {
	return errors.New("not implemented")
}
