// Package sessionlog reads Claude Code JSONL session files for
// lightweight metadata extraction (model, context usage).
package sessionlog

import "github.com/gastownhall/gascity/internal/modelwindow"

// ModelContextWindow returns the context-window size for a model ID; it
// delegates to modelwindow.Window.
func ModelContextWindow(model string) int {
	return modelwindow.Window(model)
}
