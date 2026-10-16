package tmux

import (
	"fmt"
	"strings"
)

// literalTextArgs returns the arguments that follow "send-keys -l" for tmux
// to type text byte for byte (E5).
//
// tmux's command parser reads a ";" that ends an argument as a command
// separator and turns a trailing "\;" into ";", so a final ";" gets a "\"
// before it. Its flag parser reads a leading "-" as flags, so text starting
// with "-" follows a "--".
func literalTextArgs(text string) []string {
	if strings.HasSuffix(text, ";") {
		text = text[:len(text)-1] + `\;`
	}
	if strings.HasPrefix(text, "-") {
		return []string{"--", text}
	}
	return []string{text}
}

// literalTextFromArgs reads the arguments that follow "send-keys -l" the way
// tmux does: the inverse of literalTextArgs, and an unescaped trailing ";"
// is a separator rather than text.
func literalTextFromArgs(args []string) (string, error) {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) != 1 {
		return "", fmt.Errorf("send-keys -l: needs exactly one text argument, got %d", len(args))
	}
	text := args[0]
	switch {
	case strings.HasSuffix(text, `\;`):
		return text[:len(text)-2] + ";", nil
	case strings.HasSuffix(text, ";"):
		return text[:len(text)-1], nil
	}
	return text, nil
}
