package api

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

const (
	titleGenerateTimeout = 15 * time.Second
	titleMaxTruncateLen  = 80
	titlePrompt          = `Summarize the following user message as a short conversation title (under 10 words). Output ONLY the title text, nothing else.

Message: `
)

// generateAndSetTitle runs a one-shot provider subprocess to generate a
// short title from the user's message, then updates the bead. On failure
// (unsupported provider, timeout, subprocess error) it falls back to a
// truncated version of the message.
func generateAndSetTitle(store beads.Store, beadID string, provider *config.ResolvedProvider, message, workDir string) {
	title := generateTitle(provider, message, workDir)
	title = strings.TrimSpace(title)
	if title == "" {
		return
	}
	_ = store.Update(beadID, beads.UpdateOpts{Title: &title})
}

// generateTitle invokes the provider in one-shot mode and returns a title.
// Falls back to truncating the message if the provider doesn't support
// PrintArgs or the subprocess fails.
func generateTitle(provider *config.ResolvedProvider, message, workDir string) string {
	if provider == nil || len(provider.PrintArgs) == 0 {
		return truncateTitle(message)
	}

	// Build args: <provider_args> <print_args> <model_args> <prompt+message>
	var args []string
	args = append(args, provider.Args...)
	args = append(args, provider.PrintArgs...)
	if modelArgs := provider.TitleModelFlagArgs(); len(modelArgs) > 0 {
		args = append(args, modelArgs...)
	}
	args = append(args, titlePrompt+message)

	ctx, cancel := context.WithTimeout(context.Background(), titleGenerateTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, provider.Command, args...)
	if workDir != "" {
		cmd.Dir = workDir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return truncateTitle(message)
	}

	title := strings.TrimSpace(stdout.String())
	if title == "" {
		return truncateTitle(message)
	}
	return title
}

// truncateTitle returns the first ~80 characters of message, breaking at
// a word boundary with an ellipsis appended.
func truncateTitle(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return ""
	}
	// Remove newlines for a clean single-line title.
	message = strings.ReplaceAll(message, "\n", " ")
	message = strings.Join(strings.Fields(message), " ")

	if utf8.RuneCountInString(message) <= titleMaxTruncateLen {
		return message
	}
	// Truncate to titleMaxTruncateLen runes, then back up to word boundary.
	runes := []rune(message)
	truncated := string(runes[:titleMaxTruncateLen])
	if lastSpace := strings.LastIndex(truncated, " "); lastSpace > titleMaxTruncateLen/2 {
		truncated = truncated[:lastSpace]
	}
	return truncated + "..."
}

// MaybeGenerateTitleAsync fires a goroutine to generate a title for the
// session bead if the user provided a message but no explicit title.
// It returns a channel that is closed when the background generation
// completes (or immediately if no generation is needed). Callers in
// short-lived processes (e.g. CLI) should block on the channel before
// exiting; long-lived servers can ignore it.
func MaybeGenerateTitleAsync(store beads.Store, beadID, userTitle, message string, provider *config.ResolvedProvider, workDir string, stderr func(string, ...any)) <-chan struct{} {
	done := make(chan struct{})
	message = strings.TrimSpace(message)
	if message == "" || userTitle != "" {
		close(done)
		return done
	}
	// Set the truncated message as immediate title so there's something
	// meaningful before the model responds.
	if truncated := truncateTitle(message); truncated != "" {
		_ = store.Update(beadID, beads.UpdateOpts{Title: &truncated})
	}
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				stderr("title generation panic: %v", r)
			}
		}()
		generateAndSetTitle(store, beadID, provider, message, workDir)
	}()
	return done
}
