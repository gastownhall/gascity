package opjournal

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/gastownhall/gascity/internal/promptsafe"
)

// RedeliveryNote is the one line a redelivery to a runtime that cannot
// deduplicate ends with (D-NOTE). It names nothing: no id, count or time.
const RedeliveryNote = "(this message may repeat an earlier one)"

// TextStyle is how a runtime presents a notice block.
type TextStyle string

// Text styles.
const (
	// TextReminder is the system-reminder block tmux and the hook inject;
	// labels and messages are sanitized. It is the zero value.
	TextReminder TextStyle = ""
	// TextPlain is the plain block an ACP runtime receives.
	TextPlain TextStyle = "plain"
)

// Notice block wording, byte for byte today's deferred-nudge formatters
// (cmd/gc formatNudgeInjectOutput and formatNudgeRuntimeMessage).
const (
	reminderOpen     = "<system-reminder>\n"
	reminderClose    = "</system-reminder>\n"
	reminderHeadOne  = "You have a deferred reminder that was queued until a safe boundary:\n\n"
	reminderHeadMany = "You have %d deferred reminders that were queued until a safe boundary:\n\n"
	reminderTail     = "\nHandle them after this turn.\n"
	plainHead        = "Deferred reminders:\n"
	plainTail        = "\nThese were queued until the session went idle.\n"
)

// labelOperator is the operator's principal (legacyop.PrincipalOperator),
// the one bare principal that renders as itself.
const labelOperator = "operator"

// labelFallback is the label of a principal no rule maps to a display word.
const labelFallback = "notice"

var labelRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// SourceLabel is the display word a notice from principal carries: the
// segment after gc: up to any / for gc's own principals (gc:mail → mail,
// gc:planner/drain → planner), the scheme of any other scheme:… principal
// (t3:env → t3), operator for the operator, and notice for anything else, so
// a bare identifier never reaches delivered text (I14).
func SourceLabel(principal string) string {
	scheme, rest, ok := strings.Cut(principal, ":")
	switch {
	case !ok:
		if principal == labelOperator {
			return labelOperator
		}
		return labelFallback
	case scheme == "gc":
		word, _, _ := strings.Cut(rest, "/")
		if labelRe.MatchString(word) {
			return word
		}
		return labelFallback
	case labelRe.MatchString(scheme):
		return scheme
	default:
		return labelFallback
	}
}

// RenderDelivery renders the text one delivery types: every notice as one
// block in batch order, then at most one prompt, verbatim. With note set, the
// RedeliveryNote ends the text: as the block's last line in a notice-only
// batch, otherwise on its own line after the prompt. The text carries no op
// id, client key, delivery key, lease, lane_seq, attempt or principal (I14).
// It refuses an empty batch, an op that is not a message, and two prompts.
func RenderDelivery(ops []Op, style TextStyle, note bool) (string, error) {
	if len(ops) == 0 {
		return "", errors.New("rendering a delivery: no ops")
	}
	if style != TextReminder && style != TextPlain {
		return "", fmt.Errorf("rendering a delivery: unknown text style %q", style)
	}
	havePrompt := false
	for _, op := range ops {
		if op.Kind != KindMessage {
			return "", fmt.Errorf("rendering a delivery: op %s is a %s, not a message", op.ID, op.Kind)
		}
		if op.Policy.Message.Role != RolePrompt {
			continue
		}
		if havePrompt {
			return "", fmt.Errorf("rendering a delivery: op %s is a second prompt", op.ID)
		}
		havePrompt = true
	}
	return renderDelivery(ops, style, note), nil
}

// renderDelivery is RenderDelivery's text for a batch it accepts: messages in
// a known style, at most one of them a prompt.
func renderDelivery(ops []Op, style TextStyle, note bool) string {
	var notices []Op
	prompt, havePrompt := "", false
	for _, op := range ops {
		if op.Policy.Message.Role != RolePrompt {
			notices = append(notices, op)
			continue
		}
		prompt, havePrompt = op.Payload, true
	}
	block := renderNotices(notices, style, note && !havePrompt)
	if !havePrompt {
		return block
	}
	text := prompt
	if block != "" {
		text = block + "\n" + prompt
	}
	if note {
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += "\n" + RedeliveryNote
	}
	return text
}

// renderNotices renders the notice block, "" for no notices. note puts the
// RedeliveryNote inside the block.
func renderNotices(notices []Op, style TextStyle, note bool) string {
	if len(notices) == 0 {
		return ""
	}
	var sb strings.Builder
	if style == TextPlain {
		sb.WriteString(plainHead)
		for _, op := range notices {
			fmt.Fprintf(&sb, "- [%s] %s\n", SourceLabel(op.Producer.Principal), op.Payload)
		}
		sb.WriteString(plainTail)
		if note {
			sb.WriteString(RedeliveryNote + "\n")
		}
		return sb.String()
	}
	sb.WriteString(reminderOpen)
	if len(notices) == 1 {
		sb.WriteString(reminderHeadOne)
	} else {
		fmt.Fprintf(&sb, reminderHeadMany, len(notices))
	}
	for _, op := range notices {
		label := promptsafe.SanitizeForSystemReminder(SourceLabel(op.Producer.Principal))
		message := promptsafe.SanitizeForSystemReminder(op.Payload)
		fmt.Fprintf(&sb, "- [%s] %s\n", label, message)
	}
	sb.WriteString(reminderTail)
	if note {
		sb.WriteString(RedeliveryNote + "\n")
	}
	sb.WriteString(reminderClose)
	return sb.String()
}
