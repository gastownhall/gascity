package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime/tmux"
	"github.com/spf13/cobra"
)

// The native callback supplies invocation identity before Claude flushes its
// assistant record. Failure must block PreToolUse rather than expose an
// answerable dialog with an unproven identity.
func newInternalObserveClaudeQuestionCmd(stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:    "observe-claude-question",
		Short:  "Observe one native Claude question callback",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			env := map[string]string{}
			for _, entry := range os.Environ() {
				key, value, ok := strings.Cut(entry, "=")
				if ok {
					env[key] = value
				}
			}
			if err := tmux.ObserveClaudeQuestionHook(c.Context(), c.InOrStdin(), env); err != nil {
				fmt.Fprintf(stderr, "gc native question observation: %v\n", err) //nolint:errcheck // provider callback diagnostic
				return exitForCode(2)
			}
			return nil
		},
	}
}
