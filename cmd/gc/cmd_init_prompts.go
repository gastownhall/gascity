package main

import "embed"

//go:embed prompts/*.md
var defaultPrompts embed.FS

// metaAgentAuthorPrompt is the meta-prompt used by `gc prompt synth` to
// instruct the configured provider to design an agent prompt template.
// It is rendered with [[ ]] delimiters so its body can contain literal
// {{ }} that pass through to the LLM (which then emits Gas City template
// syntax in its output).
//
//go:embed prompts/meta-agent-author.template.md
var metaAgentAuthorPrompt []byte
