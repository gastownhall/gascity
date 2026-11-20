package runtime

import (
	"errors"
	"strings"
)

// TmuxLiteralArgs returns the arguments that follow "send-keys -l" for tmux
// to type text byte for byte (E5). Every send-keys -l that carries caller
// text builds its arguments here.
//
// tmux's command parser reads a ";" that ends an argument as a command
// separator and turns a trailing "\;" into ";", so a final ";" gets a "\"
// before it. Its flag parser reads a leading "-" as flags, so text starting
// with "-" follows a "--".
func TmuxLiteralArgs(text string) []string {
	if strings.HasSuffix(text, ";") {
		text = text[:len(text)-1] + `\;`
	}
	if strings.HasPrefix(text, "-") {
		return []string{"--", text}
	}
	return []string{text}
}

// TmuxLiteralText reads the arguments that follow "send-keys -l" back into
// the text tmux types: the inverse of [TmuxLiteralArgs]. It refuses any
// argument list TmuxLiteralArgs does not produce (an unescaped final ";",
// which tmux drops as a separator, or a leading "-" without "--", which tmux
// reads as flags), so a raw send-keys -l fails loudly instead of losing text.
func TmuxLiteralText(args []string) (string, error) {
	dashed := len(args) > 0 && args[0] == "--"
	if dashed {
		args = args[1:]
	}
	if len(args) != 1 {
		return "", errors.New("send-keys -l: needs exactly one text argument")
	}
	text := args[0]
	if strings.HasPrefix(text, "-") != dashed {
		return "", errors.New(`send-keys -l: text starts with "-" exactly when it follows "--"`)
	}
	if strings.HasSuffix(text, `\;`) {
		return text[:len(text)-2] + ";", nil
	}
	if strings.HasSuffix(text, ";") {
		return "", errors.New(`send-keys -l: an unescaped final ";" is a tmux command separator`)
	}
	return text, nil
}
