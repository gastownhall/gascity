package main

import "embed"

//go:embed prompts/*.md
var defaultPrompts embed.FS
