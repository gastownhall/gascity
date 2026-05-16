package gascity

import "embed"

// BuiltinSchemas embeds the checked-in CLI JSON schemas.
//
// Logical schema paths are rooted at schemas/, for example:
// schemas/events/result.schema.json
//
//go:embed schemas/**
var BuiltinSchemas embed.FS
