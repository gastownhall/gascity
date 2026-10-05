package tmux

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Compile-time checks that both Tmux and Provider implement InteractionProvider.
var (
	_ runtime.InteractionProvider = (*Tmux)(nil)
	_ runtime.InteractionProvider = (*Provider)(nil)
)

// Pending delegates to the underlying Tmux instance.
func (p *Provider) Pending(name string) (*runtime.PendingInteraction, error) {
	return p.tm.Pending(name)
}

// Respond delegates to the underlying Tmux instance.
func (p *Provider) Respond(name string, response runtime.InteractionResponse) error {
	return p.tm.Respond(name, response)
}

// ---------------------------------------------------------------------------
// Pane-based approval detection
// ---------------------------------------------------------------------------
//
// Claude Code asks for tool permission with a dialog at the bottom of the pane.
// As of Claude Code 2.1.284 it looks like this (80 columns, default mode):
//
//	────────────────────────────────────────────────────────────────
//	 Bash command
//
//	   date > bash-ran.txt
//	   Run date command and write output to bash-ran.txt
//
//	 Do you want to proceed?
//	 ❯ 1. Yes
//	   2. Yes, and always allow access to /private/tmp/demo from
//	      this project
//	   3. No
//
//	 Esc to cancel · Tab to amend
//
// The layout drifts between releases: the tool line above the dialog blinks
// between "⏺" and a space (or is prose such as "Running date command…"), the
// "This command requires approval" line appears only in some permission
// modes, labels wrap at the pane width, and the menu can have three or four
// options. Detection therefore keys on the parts that carry meaning — a
// "Do you want to …?" question followed by a numbered menu with the ❯
// cursor, live at the bottom of the pane — and answers are chosen by option
// label, never by position. See testdata/claude-approval for real captures.

var (
	// approvalQuestionRe matches the question directly above the menu:
	// "Do you want to proceed?", "Do you want to make this edit to x?",
	// "Do you want to create x?", and the older "Approve edits?".
	approvalQuestionRe = regexp.MustCompile(`^(Do you want to .+\?|Approve edits\?)$`)

	// approvalOptionRe matches one numbered menu entry. The highlighted
	// entry carries the cursor glyph: " ❯ 1. Yes", "   2. No".
	approvalOptionRe = regexp.MustCompile(`^([❯›>]\s*)?(\d+)\.\s+(\S.*)$`)

	// toolHeaderRe matches the older tool call header "● ToolName(args)" or
	// "● ToolName" (⏺ in newer builds). The greedy match to the last ")"
	// handles nested parens in args.
	toolHeaderRe = regexp.MustCompile(`[●⏺] (\w+)(?:\((.+)\))?`)
)

// nonApprovalQuestions are dialogs that share the question-and-menu layout
// but are not tool approvals (they are handled by startup dialog dismissal).
var nonApprovalQuestions = map[string]bool{
	"Do you want to use this API key?": true,
}

// approvalDialogTools maps a permission dialog heading to the tool it gates.
// Unknown headings are reported verbatim.
var approvalDialogTools = map[string]string{
	"Bash command": "Bash",
	"Edit file":    "Edit",
	"Create file":  "Write",
}

// parsedApproval holds the parsed approval prompt from a tmux pane capture.
type parsedApproval struct {
	ToolName string
	Input    string
	Question string
	Options  []approvalOption
	// Command and Description are set for Bash prompts: the command exactly as
	// it would run (newlines kept, pane wraps rejoined) and Claude's
	// one-line description of it.
	Command     string
	Description string
}

// approvalOption is one numbered entry of the prompt's menu. Wrapped labels
// are rejoined with single spaces.
type approvalOption struct {
	Number int
	Label  string
}

// parseApprovalPrompt parses the tmux pane text for a live Claude Code
// approval prompt. It returns nil unless the pane ends with an approval
// question followed only by a well-formed numbered menu (and the optional
// "Esc to cancel" footer): a menu that has scrolled above the composer is
// history, and conversational text is not a prompt.
func parseApprovalPrompt(paneText string) *parsedApproval {
	lines := strings.Split(strings.ReplaceAll(paneText, "\r", ""), "\n")

	end := len(lines)
	for end > 0 {
		trimmed := strings.TrimSpace(lines[end-1])
		if trimmed == "" || strings.HasPrefix(trimmed, "Esc to cancel") {
			end--
			continue
		}
		break
	}

	question, menu := -1, -1
	var questionText string
	width := paneWidth(lines[:end])
	for i := end - 1; i >= 0; i-- {
		if text, next, ok := approvalQuestionAt(lines[:end], i, width); ok {
			question, menu, questionText = i, next, text
			break
		}
	}
	if question < 0 {
		return nil
	}
	if nonApprovalQuestions[questionText] {
		return nil
	}
	options, ok := parseApprovalMenu(lines[menu:end])
	if !ok {
		return nil
	}

	approval := &parsedApproval{Question: questionText, Options: options}
	describeApprovalTool(approval, lines[:question])
	return approval
}

// approvalQuestionMaxRows bounds how many pane rows one question may span.
const approvalQuestionMaxRows = 4

// approvalQuestionAt reads a permission question that starts at lines[i]. A
// long question ("Do you want to make this edit to <long file name>?") is
// hard-wrapped at the pane width, mid-word if need be, so it continues on the
// rows below until the first menu option. It returns the rejoined question,
// the index of the row after it, and whether lines[i] starts a question.
func approvalQuestionAt(lines []string, i, width int) (string, int, bool) {
	first := strings.TrimSpace(lines[i])
	if !strings.HasPrefix(first, "Do you want to ") && first != "Approve edits?" {
		return "", 0, false
	}
	// Continuation rows share the first row's indent; a space the wrap put at
	// the start of a row is part of the question.
	indent := lines[i][:len(lines[i])-len(strings.TrimLeft(lines[i], " "))]
	text := first
	prevRow := strings.TrimRight(lines[i], " ")
	next := i + 1
	for ; next < len(lines) && next-i < approvalQuestionMaxRows; next++ {
		if approvalQuestionRe.MatchString(text) {
			break
		}
		raw := strings.TrimRight(lines[next], " ")
		row := strings.TrimPrefix(raw, indent)
		if strings.TrimSpace(row) == "" || approvalOptionRe.MatchString(strings.TrimSpace(row)) {
			break
		}
		text = joinWrappedRow(text, prevRow, row, width)
		prevRow = raw
	}
	if !approvalQuestionRe.MatchString(text) {
		return "", 0, false
	}
	return text, next, true
}

// joinWrappedRow appends row, the next row of hard-wrapped text without its
// indent. A previous row that fills the pane (all but its last column) was
// cut exactly there, so row continues it as it is, a leading space included.
// A shorter one lost the space it was cut at (capture drops trailing spaces),
// so one space goes back between them.
func joinWrappedRow(text, prevRow, row string, width int) string {
	if width > 0 && utf8.RuneCountInString(prevRow) >= width-1 {
		return text + row
	}
	return text + " " + strings.TrimLeft(row, " ")
}

// paneWidth returns the pane width as the length of the widest full-width
// rule (a row of ─ or ╌) in lines, or 0 when there is none. Claude Code draws
// those rules across the whole pane.
func paneWidth(lines []string) int {
	width := 0
	for _, line := range lines {
		trimmed := strings.TrimRight(line, " ")
		if trimmed == "" || strings.Trim(trimmed, "─╌") != "" {
			continue
		}
		width = max(width, utf8.RuneCountInString(trimmed))
	}
	return width
}

// parseApprovalMenu parses the lines between the question and the end of the
// prompt. Every non-blank line must be a numbered option or the wrapped
// continuation of one; exactly one option must carry the cursor.
func parseApprovalMenu(lines []string) ([]approvalOption, bool) {
	var options []approvalOption
	seen := make(map[int]bool)
	cursors := 0
	labelColumn := 0
	for _, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		if m := approvalOptionRe.FindStringSubmatch(trimmed); m != nil {
			number, err := strconv.Atoi(m[2])
			if err != nil || seen[number] {
				return nil, false
			}
			seen[number] = true
			if m[1] != "" {
				cursors++
			}
			label := strings.TrimSpace(m[3])
			labelAt := strings.Index(raw, label)
			if labelAt < 0 {
				return nil, false
			}
			options = append(options, approvalOption{Number: number, Label: label})
			labelColumn = utf8.RuneCountInString(raw[:labelAt])
			continue
		}
		// A wrapped label continues at (or right of) the label's column.
		if len(options) == 0 || leadingSpaces(raw) < labelColumn {
			return nil, false
		}
		options[len(options)-1].Label += " " + trimmed
	}
	if len(options) < 2 || cursors != 1 {
		return nil, false
	}
	return options, true
}

func leadingSpaces(s string) int {
	return len(s) - len(strings.TrimLeft(s, " "))
}

// describeApprovalTool fills in the tool name and input shown above the
// question. The current layout opens the dialog with a horizontal rule and a
// heading ("Bash command", "Edit file", "Create file"); the older layout is
// identified by the nearest "● Tool(args)" header. Whichever is closer to
// the question belongs to this prompt. When neither is present the question
// itself stands in, so the prompt is still reported and still blocks input.
func describeApprovalTool(approval *parsedApproval, before []string) {
	rule := -1
	for i := len(before) - 1; i >= 0; i-- {
		if isDialogRule(before[i]) {
			rule = i
			break
		}
	}
	header := -1
	var headerMatch []string
	for i := len(before) - 1; i >= 0; i-- {
		if m := toolHeaderRe.FindStringSubmatch(before[i]); m != nil {
			header, headerMatch = i, m
			break
		}
	}

	if rule >= 0 && rule > header {
		body := before[rule+1:]
		for i, line := range body {
			title := strings.TrimSpace(line)
			if title == "" {
				continue
			}
			approval.ToolName = title
			if tool, ok := approvalDialogTools[title]; ok {
				approval.ToolName = tool
			}
			if approval.ToolName == "Bash" {
				if command, description, ok := boxedBashSubject(body[i+1:]); ok {
					approval.Command, approval.Description = command, description
					approval.Input = truncateApprovalInput(strings.TrimSuffix(command+"\n"+description, "\n"))
					return
				}
			}
			approval.Input = dialogSubject(body[i+1:], approval.ToolName)
			if approval.ToolName == "Bash" && approval.Input != "" {
				// The older layout: the command's line first, its description after.
				first, rest, _ := strings.Cut(approval.Input, "\n")
				approval.Command = first
				approval.Description = strings.Join(strings.Fields(rest), " ")
			}
			return
		}
	}
	if header >= 0 {
		approval.ToolName = headerMatch[1]
		if len(headerMatch) >= 3 && headerMatch[2] != "" {
			approval.Input = headerMatch[2]
		} else {
			approval.Input = extractToolInput(strings.Join(before, "\n"), approval.ToolName)
		}
		return
	}
	approval.Input = approval.Question
}

// isDialogRule reports whether line is the full-width rule that opens a
// dialog.
func isDialogRule(line string) bool {
	trimmed := strings.TrimSpace(line)
	return utf8.RuneCountInString(trimmed) >= 10 && strings.Trim(trimmed, "─") == ""
}

// dialogSubject extracts what the dialog asks about from the lines between
// its heading and the question. Older Bash dialogs show the command (and
// Claude's description of it) as an indented block, command first; file
// dialogs show the file path on the first line, above the ╌ diff separator.
// The boxed Bash layout of Claude Code 2.1.287+ is read by boxedBashSubject.
func dialogSubject(lines []string, _ string) string {
	var header []string
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "╌") {
			break
		}
		header = append(header, line)
	}

	var block []string
	for _, line := range header {
		if strings.TrimSpace(line) == "" || leadingSpaces(line) < 3 {
			if len(block) > 0 {
				break
			}
			continue
		}
		block = append(block, strings.TrimSpace(line))
	}
	if len(block) == 0 {
		for _, line := range header {
			if trimmed := strings.TrimSpace(line); trimmed != "" {
				block = append(block, trimmed)
				break
			}
		}
	}
	return truncateApprovalInput(strings.Join(block, "\n"))
}

// boxedBashSubject reads the Bash layout of Claude Code 2.1.287+: Claude's
// description, then the command between two ╌ separator lines. It returns the
// command and the description, and false when the lines hold no such box.
//
// From 2.1.288, every line of a box that spans several lines starts with
// "│ ", whether the line ends at a real newline or where the pane wrapped it,
// and the description gets the same marker when it wraps. A marked line is a
// wrap when the next line's first word would not have fit after it; then the
// two are rejoined with a space. Otherwise the newline is real and kept. So
// the command reads exactly as it would run, and it doesn't depend on the
// pane's width.
func boxedBashSubject(lines []string) (string, string, bool) {
	isSep := func(line string) bool { return strings.HasPrefix(strings.TrimSpace(line), "╌") }
	open := -1
	for i, line := range lines {
		if isSep(line) {
			open = i
			break
		}
	}
	if open < 0 {
		return "", "", false
	}
	// The separator spans the pane; marked content wraps 4 columns short of it
	// (measured on 2.1.288 captures at 80 and 100 columns).
	width := utf8.RuneCountInString(strings.TrimRight(lines[open], " ")) - 4
	var box []string
	closed := false
	for _, line := range lines[open+1:] {
		if isSep(line) {
			closed = true
			break
		}
		box = append(box, line)
	}
	command := unwrapMarkedLines(box, width)
	if !closed || len(command) == 0 {
		return "", "", false
	}
	description := unwrapMarkedLines(lines[:open], width)
	return strings.Join(command, "\n"), strings.Join(description, " "), true
}

// unwrapMarkedLines returns the non-blank lines with their "│ " markers removed
// and pane wraps rejoined (see boxedBashSubject). Unmarked lines are kept as
// they are, trimmed.
func unwrapMarkedLines(lines []string, width int) []string {
	var out []string
	prevMarked := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			prevMarked = false
			continue
		}
		marked := strings.HasPrefix(trimmed, "│")
		content := trimmed
		if marked {
			content = strings.TrimSpace(strings.TrimPrefix(trimmed, "│"))
		}
		if marked && prevMarked && len(out) > 0 {
			prev := out[len(out)-1]
			firstWord := content
			if i := strings.IndexByte(content, ' '); i >= 0 {
				firstWord = content[:i]
			}
			if utf8.RuneCountInString(prev)+1+utf8.RuneCountInString(firstWord) > width {
				out[len(out)-1] = prev + " " + content
				continue
			}
		}
		out = append(out, content)
		prevMarked = marked
	}
	return out
}

func truncateApprovalInput(s string) string {
	if len(s) > 500 {
		return s[:500] + "…"
	}
	return s
}

// approvalOptionKey returns the keystroke that selects the menu option for
// action, chosen by label: "approve" is the option labeled "Yes", "deny" the
// one labeled "No…", "approve_always" the "Yes, and don't ask again …" /
// "Yes, and always allow …" option, and "approve_accept_edits" the option
// that switches to accept-edits mode. Unless exactly one option matches, it
// returns an error wrapping runtime.ErrInteractionActionUnavailable so the
// caller sends nothing rather than guess.
func approvalOptionKey(approval *parsedApproval, action string) (string, error) {
	var match func(label string) bool
	switch action {
	case "approve":
		match = func(label string) bool { return label == "yes" }
	case "approve_always":
		match = func(label string) bool {
			return strings.HasPrefix(label, "yes, and don't ask again") ||
				strings.HasPrefix(label, "yes, and always allow")
		}
	case "approve_accept_edits":
		match = func(label string) bool {
			return strings.HasPrefix(label, "yes") &&
				(strings.Contains(label, "accept edits") || strings.Contains(label, "don't ask again for edits"))
		}
	case "deny":
		match = func(label string) bool {
			return label == "no" || strings.HasPrefix(label, "no,") || strings.HasPrefix(label, "no ")
		}
	default:
		return "", fmt.Errorf("unknown action %q", action)
	}

	var found []approvalOption
	for _, option := range approval.Options {
		if match(normalizeApprovalLabel(option.Label)) {
			found = append(found, option)
		}
	}
	labels := make([]string, 0, len(approval.Options))
	for _, option := range approval.Options {
		labels = append(labels, fmt.Sprintf("%d. %s", option.Number, option.Label))
	}
	if len(found) != 1 {
		return "", fmt.Errorf("%w: %d menu options match action %q (menu: %s)",
			runtime.ErrInteractionActionUnavailable, len(found), action, strings.Join(labels, " | "))
	}
	if found[0].Number < 1 || found[0].Number > 9 {
		return "", fmt.Errorf("%w: option %d for action %q cannot be selected with one key (menu: %s)",
			runtime.ErrInteractionActionUnavailable, found[0].Number, action, strings.Join(labels, " | "))
	}
	return strconv.Itoa(found[0].Number), nil
}

func normalizeApprovalLabel(label string) string {
	label = strings.ReplaceAll(label, "’", "'")
	return strings.ToLower(strings.Join(strings.Fields(label), " "))
}

// extractToolInput extracts the indented tool input block from pane text.
// Older Claude builds show tool input as indented lines between the
// "● ToolName" header and the approval question. Searches backwards from the
// end of textBeforeApproval to find the last tool header occurrence.
func extractToolInput(textBeforeApproval, toolName string) string {
	lines := strings.Split(textBeforeApproval, "\n")

	// Find the last line containing the tool header
	headerIdx := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], "● "+toolName) || strings.Contains(lines[i], "⏺ "+toolName) {
			headerIdx = i
			break
		}
	}
	if headerIdx < 0 {
		return ""
	}

	var captured []string
	for _, line := range lines[headerIdx+1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			break
		}
		// Skip UI decoration lines (spinners, box-drawing, etc.)
		if strings.HasPrefix(trimmed, "⎿") || strings.HasPrefix(trimmed, "───") ||
			strings.HasPrefix(trimmed, "│") || trimmed == "Running…" {
			continue
		}
		// Claude indents tool input with leading spaces
		if strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t") {
			captured = append(captured, trimmed)
		}
	}

	if len(captured) == 0 {
		return ""
	}
	return truncateApprovalInput(strings.Join(captured, "\n"))
}

// ---------------------------------------------------------------------------
// Deduplication
// ---------------------------------------------------------------------------

// Per-session dedup state to avoid re-emitting the same approval.
type approvalDedup struct {
	mu       sync.Mutex
	lastHash map[string]string // session name → hash of last emitted approval
}

func approvalHash(a *parsedApproval) string {
	h := sha256.Sum256([]byte(a.ToolName + "\x00" + a.Input))
	return fmt.Sprintf("%x", h[:8])
}

func (d *approvalDedup) isNew(session string, a *parsedApproval) bool {
	hash := approvalHash(a)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lastHash[session] == hash {
		return false
	}
	d.lastHash[session] = hash
	return true
}

func (d *approvalDedup) clear(session string) {
	d.mu.Lock()
	delete(d.lastHash, session)
	d.mu.Unlock()
}

// ---------------------------------------------------------------------------
// InteractionProvider implementation
// ---------------------------------------------------------------------------

// Pending checks the tmux pane for an active Claude Code approval prompt.
// Returns nil with no error if no approval is pending.
func (t *Tmux) Pending(name string) (*runtime.PendingInteraction, error) {
	paneText, err := t.CapturePane(name, 40)
	if err != nil {
		// Pane might not exist (session not started yet or already stopped).
		// Check for known "can't find" errors vs unexpected failures.
		// A server that answered with no sessions (ErrNoCurrentTarget) proves
		// the pane is gone. Any other ErrNoServer ("no tmux server running",
		// "error connecting to") is a failed observation, not proof of absence,
		// as in ListRunning. Its message omits the tmux text, which matches
		// runtime.IsSessionGone and would read "unknown" as "gone".
		if errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrNoCurrentTarget) {
			return nil, fmt.Errorf("capturing pane: %w: %w", runtime.ErrSessionNotFound, err)
		}
		if errors.Is(err, ErrNoServer) {
			return nil, &quietCauseError{
				msg:  fmt.Sprintf("capturing pane of %q: tmux server unreachable: %v", name, runtime.ErrRuntimeUnavailable),
				errs: []error{runtime.ErrRuntimeUnavailable, err},
			}
		}
		if strings.Contains(err.Error(), "can't find") {
			return nil, nil
		}
		return nil, fmt.Errorf("capturing pane: %w", err)
	}

	approval := parseApprovalPrompt(paneText)
	if approval == nil {
		t.approvalDedup().clear(name)
		return nil, nil
	}

	// Dedup: don't re-emit the same approval on repeated polls.
	if !t.approvalDedup().isNew(name, approval) {
		// Return the interaction (caller may need it for display) but it's
		// not a new detection. The stable RequestID makes this idempotent.
		_ = struct{}{} // satisfy empty-block linter; dedup check is intentionally a no-op
	}

	requestID := "tmux-" + approvalHash(approval)
	metadata := map[string]string{"source": "tmux"}
	if approval.ToolName != "" {
		metadata["tool_name"] = approval.ToolName
	}
	if approval.Command != "" {
		metadata["command"] = approval.Command
	}
	if approval.Description != "" {
		metadata["description"] = approval.Description
	}
	return &runtime.PendingInteraction{
		RequestID: requestID,
		Kind:      "approval",
		Prompt:    approvalPromptText(approval),
		Options:   approvalOptionLabels(approval),
		Metadata:  metadata,
	}, nil
}

// approvalPromptText renders the one-line description a client shows for the
// pending approval.
func approvalPromptText(approval *parsedApproval) string {
	switch {
	case approval.ToolName == "":
		return approval.Question
	case approval.Input == "":
		return "Allow " + approval.ToolName + "?"
	default:
		return approval.ToolName + ": " + approval.Input
	}
}

// approvalOptionLabels returns the menu labels in pane order.
func approvalOptionLabels(approval *parsedApproval) []string {
	labels := make([]string, 0, len(approval.Options))
	for _, option := range approval.Options {
		labels = append(labels, option.Label)
	}
	return labels
}

// checkNoApprovalPrompt returns an error wrapping runtime.ErrPendingInteraction
// when the pane is showing a permission prompt. Text typed into the prompt
// would be read as menu input — Enter picks the highlighted "Yes" and a digit
// picks that option — so delivery paths call this before sending any text.
// Capture failures are returned as-is for the caller to classify.
func (t *Tmux) checkNoApprovalPrompt(name string) error {
	return t.checkNoApprovalPromptIn(name, name)
}

// checkNoApprovalPromptIn is checkNoApprovalPrompt for session name's agent
// pane target (a pane id in a multi-pane session).
func (t *Tmux) checkNoApprovalPromptIn(name, target string) error {
	paneText, err := t.CapturePane(target, 40)
	if err != nil {
		return err
	}
	approval := parseApprovalPrompt(paneText)
	if approval == nil {
		return nil
	}
	return fmt.Errorf("%w: session %q is waiting on a permission prompt (%s, request %s); answer it with respond (approve or deny) before sending text",
		runtime.ErrPendingInteraction, name, approvalPromptText(approval), "tmux-"+approvalHash(approval))
}

// Respond's verify polling; variables so tests can shrink them. Under load
// Claude Code can take seconds to redraw after the answer key, so Respond keeps
// checking until the deadline rather than for a fixed number of tries.
var (
	respondVerifyInterval = 250 * time.Millisecond
	respondVerifyTimeout  = 10 * time.Second
)

// Respond sends the appropriate keystroke to the tmux pane to approve or deny
// a pending tool approval, then verifies the prompt was consumed.
func (t *Tmux) Respond(name string, response runtime.InteractionResponse) error {
	// Verify the expected approval is still present before sending keys.
	paneText, err := t.CapturePane(name, 40)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return fmt.Errorf("pre-verify capture failed: %w: %w", runtime.ErrSessionNotFound, err)
		}
		return fmt.Errorf("pre-verify capture failed: %w", err)
	}
	current := parseApprovalPrompt(paneText)
	if current == nil {
		t.approvalDedup().clear(name)
		return nil // prompt already gone
	}
	// If caller specified a RequestID, verify it matches the current prompt.
	if response.RequestID != "" {
		currentID := "tmux-" + approvalHash(current)
		if currentID != response.RequestID {
			return fmt.Errorf("approval prompt changed: expected %s, got %s", response.RequestID, currentID)
		}
	}

	key, err := approvalOptionKey(current, response.Action)
	if err != nil {
		return err
	}

	// Exit copy-mode first if the pane is parked (the ga-c4w wheel binding),
	// so the approval keystroke reaches the prompt instead of being swallowed
	// by copy-mode.
	t.cancelCopyModeIfParked(name)

	// Send the keystroke once.
	if _, err := t.run("send-keys", "-t", paneTarget(name), "-l", key); err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return fmt.Errorf("send-keys failed: %w: %w", runtime.ErrSessionNotFound, err)
		}
		return fmt.Errorf("send-keys failed: %w", err)
	}

	// Poll until the deadline to verify the prompt cleared. Do NOT re-send
	// the keystroke — if Claude is slow to process, re-sending would type into
	// whatever comes next (message input or a subsequent approval).
	deadline := time.Now().Add(respondVerifyTimeout)
	for time.Now().Before(deadline) {
		time.Sleep(respondVerifyInterval)

		verifyText, verifyErr := t.CapturePane(name, 40)
		if verifyErr != nil {
			// Pane gone — session ended, treat as success.
			t.approvalDedup().clear(name)
			return nil
		}

		// Success once the prompt we answered is gone, even if the next
		// tool call has already raised a different one.
		if next := parseApprovalPrompt(verifyText); next == nil || approvalHash(next) != approvalHash(current) {
			t.approvalDedup().clear(name)
			return nil
		}
	}

	return fmt.Errorf("approval prompt did not clear within %s of the answer", respondVerifyTimeout)
}
