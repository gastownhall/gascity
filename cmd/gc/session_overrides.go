package main

import (
	"github.com/gastownhall/gascity/internal/config"
)

// replaceSchemaFlags strips all CLI flags associated with the provider's
// OptionsSchema from the command, then appends the given override flags.
func replaceSchemaFlags(command string, schema []config.ProviderOption, overrideArgs []string) string {
	return config.ReplaceSchemaFlags(command, schema, overrideArgs)
}

// stripFlags removes known flag sequences from a tokenized command.
func stripFlags(command string, flags [][]string) string {
	return config.StripFlags(command, flags)
}
