package runtime

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnrecognizedWorkspaceTrust means a Claude workspace trust dialog is
// visible but its safe selection cannot be established. Launch must stop
// before sending any readiness or startup input into that dialog.
var ErrUnrecognizedWorkspaceTrust = errors.New("unrecognized Claude workspace trust menu")

// workspaceTrustDialogKeys retains the existing startup trust policy. Claude
// must display its specific two-choice menu before we navigate to its explicit
// trust option; unknown or incomplete menus are left untouched.
func workspaceTrustDialogKeys(content string) ([]string, error) {
	if !strings.Contains(content, "Quick safety check") && !strings.Contains(content, "trust this folder") {
		return []string{"Enter"}, nil // Existing Codex, Gemini, and pi default choices.
	}
	invalid := ErrUnrecognizedWorkspaceTrust
	const footer = "Enter to confirm · Esc to cancel"
	before, after, ok := strings.Cut(content, footer)
	if !ok || strings.TrimSpace(after) != "" {
		return nil, invalid
	}
	lines := strings.Split(strings.TrimSpace(before), "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	block := strings.Join(lines, "\n")
	if at := strings.LastIndex(block, "\n\n"); at >= 0 {
		block = block[at+2:]
	}
	options := strings.Split(block, "\n")
	if len(options) != 2 {
		return nil, invalid
	}
	selected, trust := -1, -1
	for i, line := range options {
		if strings.HasPrefix(line, "❯ ") {
			if selected >= 0 {
				return nil, invalid
			}
			selected = i
			line = strings.TrimSpace(strings.TrimPrefix(line, "❯"))
		}
		// Earlier Claude versions numbered this same menu in display order.
		line = strings.TrimPrefix(line, fmt.Sprintf("%d. ", i+1))
		switch line {
		case "Yes, I trust this folder":
			if trust >= 0 {
				return nil, invalid
			}
			trust = i
		case "No, exit":
		default:
			return nil, invalid
		}
	}
	if selected < 0 || trust < 0 {
		return nil, invalid
	}
	switch {
	case selected < trust:
		return []string{"Down", "Enter"}, nil
	case selected > trust:
		return []string{"Up", "Enter"}, nil
	default:
		return []string{"Enter"}, nil
	}
}
