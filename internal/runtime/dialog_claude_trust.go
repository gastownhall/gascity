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

// workspaceTrustDialogKeys keeps the shared provider-specific selection logic,
// but prevents an unrecognized Claude menu from falling through to readiness.
func workspaceTrustDialogKeys(content string) ([]string, error) {
	keys, ok := workspaceTrustConfirmKeys(content)
	if ok {
		return keys, nil
	}
	if strings.Contains(content, "Quick safety check") || strings.Contains(content, "trust this folder") {
		return nil, ErrUnrecognizedWorkspaceTrust
	}
	return nil, nil
}

// validClaudeWorkspaceTrust bounds selection to the complete, known two-option
// menu. Navigation remains owned by deriveTrustDialogKeys for all providers.
func validClaudeWorkspaceTrust(content string) bool {
	lines := strings.Split(content, "\n")
	for i := range lines {
		lines[i] = stripLeadingBoxBorder(strings.TrimSpace(lines[i]))
	}
	content = strings.Join(lines, "\n")
	const footer = "Enter to confirm · Esc to cancel"
	before, after, ok := strings.Cut(content, footer)
	if !ok || strings.TrimSpace(after) != "" {
		return false
	}
	block := strings.TrimSpace(before)
	if at := strings.LastIndex(block, "\n\n"); at >= 0 {
		block = block[at+2:]
	}
	options := strings.Split(block, "\n")
	if len(options) != 2 {
		return false
	}
	selected, trust, exit := 0, 0, 0
	for i, line := range options {
		if strings.HasPrefix(line, "❯ ") {
			selected++
			line = strings.TrimSpace(strings.TrimPrefix(line, "❯"))
		}
		line = strings.TrimPrefix(line, fmt.Sprintf("%d. ", i+1))
		switch line {
		case "Yes, I trust this folder":
			trust++
		case "No, exit":
			exit++
		default:
			return false
		}
	}
	return selected == 1 && trust == 1 && exit == 1
}
