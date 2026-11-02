package tmux

import (
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// Codex approval overlay
// ---------------------------------------------------------------------------
//
// Codex asks for approval with an overlay that replaces its composer at the
// bottom of the pane (codex-rs/tui/src/bottom_pane/approval_overlay.rs). As of
// codex-cli 0.162.1 it looks like this (80 columns):
//
//	  Would you like to run the following command?
//
//	  Environment: local
//
//	  Reason: May I write the requested two-line file to
//	  /tmp/codex-approval-b955b7.txt? The current environment is read-only.
//
//	  $ printf "%s\n" one two > /tmp/codex-approval-b955b7.txt
//
//
//	› 1. Yes, proceed (y)
//	  2. Yes, and don't ask again for commands that start with `printf "%s\n" one
//	     two > /tmp/codex-approval-b955b7.txt` (p)
//	  3. No, and tell Codex what to do differently (esc)
//
//	  Press enter to confirm or esc to cancel
//
// Unlike Claude's dialog, a header sits between the title and the menu, each
// label ends with its shortcut key, and the title and footer wrap on a narrow
// pane. Every overlay row is indented two columns except the highlighted
// option, so a row above the menu that is indented less (the "› …" user
// message, a "• Ran …" history line) means the question above it is
// history. The highlighted option also starts with "› ", Codex's
// ready-prompt prefix, so an overlay gc does not recognize reads as an idle
// composer. Digit keys select an option, as in Claude's menu. See
// testdata/codex-approval for real captures.
//
// The header ends with the request itself: the command ("$ …", one row per
// command line and per pane wrap), the input for a running terminal, the
// permission rule, or one "Destination: …" row per file. Codex prints it
// verbatim, so it can hold rows that look like a title or a header field. An
// overlay that shows more than one title, or a command row above its title,
// is reported without a title or subject, and gc answers it only with deny.
// gc also answers only with deny an overlay that does not show its request,
// or whose header was cut short with a "[… N lines]" row because the pane is
// too small to show it all.
//
// gc reads an overlay from a capture of the screen and up to
// PromptObservationLines rows of history above it, taken with the pane's
// width and history size. It trusts the first title it finds only when the
// capture shows where the overlay starts: a row above it indented less than
// its rows, or the top of the pane's history. Otherwise the capture may start
// inside the overlay, below its own title, where a title-shaped row of the
// request would read as the overlay's; gc reports such an overlay without a
// subject and answers it only with deny. So it does an overlay under a title
// gc does not know, when the footer shows that keys typed now would answer
// it.
//
// Codex wraps an overlay's text to the pane width less four columns,
// breaking at spaces and cutting a word too long for a row. gc rejoins the
// rows for the request ID (codexUnwrapRows), so a request keeps its ID when
// the pane is resized: a break where the next word would not have fit on the
// row above reads as a space, and a full row of one word reads as a word cut
// there. Every other break is a line break of the request, as is a break
// next to a blank row, one before a row that starts with a space, and one
// next to a row of text other than plain ASCII, whose width gc does not
// count. A request other than a command is shown rejoined the same way.
//
// The screen cannot show a line break that falls where Codex would have
// wrapped, or how many spaces a wrap replaced, and a command's author can put
// such a line break on a pane of any width: one before a word too long for a
// row draws the rows a space there would, and one after a word that fills a
// row (76 columns on gc's default 80-column pane) draws the rows of no break
// at all, which gc reads as a word cut there. Codex colors the command, which
// might tell the two apart, but gc captures the pane as plain text. Such
// commands share a request ID, so Respond's check that the pane still shows
// the request an answer names holds for a line break only before a word that
// would have fit on the row above. gc therefore shows a command as Codex drew
// it, one row per line: a command that only wrapped reads as more lines than
// it has, which costs at most a needless deny, and, while the pane keeps its
// width, an approval consents to the rows the approver saw. A resize between
// reading and answering can let the request ID the approver read match a
// request whose rows break differently.

var (
	// codexApprovalQuestionRe matches the overlay titles that ask for
	// approval of a command, a network host, a file edit, a permission grant,
	// or input to a running terminal.
	codexApprovalQuestionRe = regexp.MustCompile(`^(Would you like to (run the following command|make the following edits|grant these permissions|send input to .+)|Do you want to approve network access to .+)\?$`)

	// codexNetworkHostRe extracts the host from a network approval title.
	codexNetworkHostRe = regexp.MustCompile(`^Do you want to approve (network access to .+)\?$`)

	// codexShortcutHintRe matches the shortcut Codex appends to a menu label:
	// " (y)", " (esc)".
	codexShortcutHintRe = regexp.MustCompile(`\s+\([^()\s][^()]{0,15}\)$`)

	// codexFooterRe matches the footer under the menu, which names the keys
	// as configured: "Press enter to confirm or esc to cancel", optionally
	// followed by " or <key> to open thread".
	codexFooterRe = regexp.MustCompile(`^Press \S.* to (confirm|cancel|open thread)$`)

	// codexCollapsedHeaderRe matches the row that replaces the end of a header
	// too tall for the pane: "[… 43 lines] ⌃g view all".
	codexCollapsedHeaderRe = regexp.MustCompile(`^\[… \d+ lines\]( .+ view all)?$`)
)

// codexFooterMaxRows bounds how many rows a wrapped footer may span.
const codexFooterMaxRows = 3

// Why gc refuses to answer an overlay with anything but deny.
const (
	codexUnreadableTitles    = "the overlay shows more than one approval title, so gc cannot tell which request it is"
	codexUnreadableCommand   = "a command row sits above the approval title, so the title may be part of a request's text"
	codexUnreadableCollapsed = "the overlay is collapsed, so part of the request is hidden; enlarge the pane to read it"
	codexUnreadableMissing   = "the overlay does not show the request"
	codexUnreadableTop       = "the overlay's top is not in view, so gc cannot tell its title from request text"
	codexUnreadableUntitled  = "the overlay's title is not one gc knows"
)

// codexUnknownQuestion is the question gc reports for an overlay whose title
// it does not read.
const codexUnknownQuestion = "Codex is asking for approval"

// codexOverlayInset is how many columns Codex indents an overlay's rows from
// each side of the pane.
const codexOverlayInset = 2

// parseCodexApprovalOverlay parses a live Codex approval overlay at the bottom
// of the pane, laid out for a pane width columns wide (0 when unknown);
// fromTop reports whether lines start at the top of the pane's history. It
// returns nil unless the pane ends with a well-formed numbered menu (and the
// optional footer) under an approval title and its indented header, or under
// the footer.
func parseCodexApprovalOverlay(lines []string, width int, fromTop bool) *parsedApproval {
	end, footer := codexOverlayEnd(lines)
	menu := codexMenuStart(lines, end)
	if menu < 0 {
		return nil
	}
	options, ok := parseCodexMenu(lines[menu:end])
	if !ok {
		return nil
	}

	// The overlay owns the blank and indented rows above the menu. The title
	// is the first of them that reads as one; the request is printed below it,
	// so any other title-shaped row is request text. Which row is the first
	// is known only when the capture shows where the overlay starts.
	top, bounded := codexOverlayTop(lines, menu)
	titles := codexApprovalTitles(lines[:menu], top)
	switch {
	case len(titles) == 0 && !footer:
		return nil
	case !bounded && !fromTop:
		return codexUnreadableOverlay(options, codexUnreadableTop, lines[top:menu], width)
	case len(titles) == 0:
		return codexUnreadableOverlay(options, codexUnreadableUntitled, lines[top:menu], width)
	}
	if reason := codexForgedTitle(lines[top:titles[0]], len(titles)); reason != "" {
		return codexUnreadableOverlay(options, reason, lines[top:menu], width)
	}
	question, next, _ := codexApprovalTitleAt(lines[:menu], titles[0])
	approval := &parsedApproval{
		Question:    question,
		Options:     options,
		Codex:       true,
		Fingerprint: codexUnwrapRows(lines[titles[0]:trimBlankTail(lines, menu)], width),
	}
	describeCodexApproval(approval, lines[next:menu], width)
	return approval
}

// codexOverlayEnd returns the index after the overlay's last menu row: the
// end of the pane without its blank rows and the footer ("Press enter to
// confirm or esc to cancel", wrapped on a narrow pane). It also reports
// whether the pane ends with the footer.
func codexOverlayEnd(lines []string) (int, bool) {
	end := trimBlankTail(lines, len(lines))
	footer := ""
	for i := end - 1; i >= 0 && i >= end-codexFooterMaxRows; i-- {
		row := strings.TrimSpace(lines[i])
		if row == "" {
			break
		}
		footer = strings.TrimSpace(row + " " + footer)
		if codexFooterRe.MatchString(footer) {
			return trimBlankTail(lines, i), true
		}
	}
	return end, false
}

// codexMenuStart returns the index of the menu's first row, the last row
// before end that starts at option 1, or -1 when there is none.
func codexMenuStart(lines []string, end int) int {
	for i := end - 1; i >= 0; i-- {
		if m := approvalOptionRe.FindStringSubmatch(strings.TrimSpace(lines[i])); m != nil && m[2] == "1" {
			return i
		}
	}
	return -1
}

// parseCodexMenu parses the menu rows, dropping the shortcut key Codex
// appends to each label.
func parseCodexMenu(rows []string) ([]approvalOption, bool) {
	options, ok := parseApprovalMenu(rows)
	if !ok {
		return nil, false
	}
	for i := range options {
		options[i].Label = codexShortcutHintRe.ReplaceAllString(options[i].Label, "")
	}
	return options, true
}

// codexOverlayTop returns the index of the first row the overlay above the
// menu can span: the row after the nearest one indented less than the
// overlay's rows, which is history. It reports whether it found that row;
// without one, the overlay may start above lines.
func codexOverlayTop(lines []string, menu int) (int, bool) {
	for i := menu - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" && leadingSpaces(lines[i]) < codexOverlayInset {
			return i + 1, true
		}
	}
	return 0, false
}

// codexApprovalTitles returns the indexes of the rows from top on that start
// an approval title.
func codexApprovalTitles(lines []string, top int) []int {
	var titles []int
	for i := top; i < len(lines); i++ {
		if _, _, ok := codexApprovalTitleAt(lines, i); ok {
			titles = append(titles, i)
		}
	}
	return titles
}

// codexUnreadableOverlay reports an overlay gc answers only with deny, for
// reason, with no title or subject; rows, the overlay's rows above its menu,
// make up its request ID.
func codexUnreadableOverlay(options []approvalOption, reason string, rows []string, width int) *parsedApproval {
	return &parsedApproval{
		Question:    codexUnknownQuestion,
		Options:     options,
		Codex:       true,
		Unreadable:  reason,
		Fingerprint: codexUnwrapRows(rows[:trimBlankTail(rows, len(rows))], width),
	}
}

// codexForgedTitle returns why the overlay's title cannot be trusted to be
// its own, or "" when it can: Codex prints the title first, so another title
// below it, or a command row above it, means request text is posing as one.
func codexForgedTitle(above []string, titles int) string {
	if titles > 1 {
		return codexUnreadableTitles
	}
	for _, line := range above {
		if codexRowHasLabel(strings.TrimSpace(line), "$") {
			return codexUnreadableCommand
		}
	}
	return ""
}

// codexApprovalTitleAt reads an approval title that starts at lines[i],
// rejoining the rows a narrow pane wrapped it onto; a blank row follows it.
// It returns the title, the index of the row after it, and whether lines[i]
// starts one.
func codexApprovalTitleAt(lines []string, i int) (string, int, bool) {
	text := strings.TrimSpace(lines[i])
	if !strings.HasPrefix(text, "Would you like to ") && !strings.HasPrefix(text, "Do you want to approve network access") {
		return "", 0, false
	}
	next := i + 1
	for ; next < len(lines) && !codexApprovalQuestionRe.MatchString(text); next++ {
		row := strings.TrimSpace(lines[next])
		if row == "" {
			break
		}
		text += " " + row
	}
	if !codexApprovalQuestionRe.MatchString(text) {
		return "", 0, false
	}
	return text, next, true
}

// codexLayout is how one kind of approval lays out its header.
type codexLayout struct {
	// tool is the tool name gc reports.
	tool string
	// request labels the header's last field, which holds the request ("$"
	// for a command), or is "" when the title names the request.
	request string
	// description labels the field that explains the request.
	description string
}

// codexLayoutFor returns the header layout of the approval titled question.
func codexLayoutFor(question string) codexLayout {
	switch {
	case strings.HasPrefix(question, "Would you like to make the following edits"):
		return codexLayout{tool: "apply_patch", request: "Destination:", description: "Description:"}
	case strings.HasPrefix(question, "Would you like to grant these permissions"):
		return codexLayout{tool: "request_permissions", request: "Permission rule:", description: "Reason:"}
	case strings.HasPrefix(question, "Would you like to send input to "):
		return codexLayout{tool: "write_stdin", request: "Input:", description: "Reason:"}
	case strings.HasPrefix(question, "Do you want to approve network access"):
		return codexLayout{tool: "exec", description: "Reason:"}
	default:
		return codexLayout{tool: "exec", request: "$", description: "Reason:"}
	}
}

// describeCodexApproval fills in the tool name and subject from the title and
// the header rows between it and the menu, which Codex wrapped for a pane
// width columns wide. A header cut short, or one that does not show the
// request, leaves the overlay to be answered only with deny.
func describeCodexApproval(approval *parsedApproval, body []string, width int) {
	layout := codexLayoutFor(approval.Question)
	header := parseCodexHeader(body, layout.request, width)
	approval.ToolName = layout.tool
	approval.Description = header.fields[layout.description]
	approval.Input = header.request
	if layout.request == "$" {
		approval.Command = header.request
	}
	if m := codexNetworkHostRe.FindStringSubmatch(approval.Question); m != nil {
		approval.Input = m[1]
	}
	switch {
	case header.collapsed != "":
		approval.Unreadable = codexUnreadableCollapsed
		if approval.Input == "" {
			approval.Input = header.collapsed
		}
	case layout.request != "" && !header.found:
		approval.Unreadable = codexUnreadableMissing
	}
	approval.Input = truncateApprovalInput(approval.Input)
}

// codexHeader is the overlay header between the title and the menu.
type codexHeader struct {
	// fields holds the labeled fields above the request, keyed by label
	// ("Reason:"), with their wrapped rows rejoined.
	fields map[string]string
	// request is the request field's text, ending with the collapsed row
	// when the header was cut short.
	request string
	// found reports whether the request field is on the pane.
	found bool
	// collapsed is the "[… N lines]" row that replaced the header's end.
	collapsed string
}

// parseCodexHeader splits the header rows into its fields. Codex prints the
// request last, so the request field runs from its labeled row to the end of
// the header, blank rows and all; the fields above it are separated by blank
// rows.
func parseCodexHeader(body []string, requestLabel string, width int) codexHeader {
	var header codexHeader
	end := trimBlankTail(body, len(body))
	if end > 0 && codexCollapsedHeaderRe.MatchString(strings.TrimSpace(body[end-1])) {
		header.collapsed = strings.TrimSpace(body[end-1])
		end = trimBlankTail(body, end-1)
	}
	start := end
	for i := 0; requestLabel != "" && i < end; i++ {
		if codexRowHasLabel(strings.TrimSpace(body[i]), requestLabel) {
			start = i
			break
		}
	}
	header.fields = codexHeaderFields(body[:start])
	if start < end {
		header.found = true
		header.request = codexRequestText(body[start:end], requestLabel, width)
		if header.collapsed != "" {
			header.request += "\n" + header.collapsed
		}
	}
	return header
}

// codexHeaderLabels are the labels that open the header's fields.
var codexHeaderLabels = []string{"Thread:", "Environment:", "Reason:", "Permission rule:", "Description:", "Destination:", "Input:"}

// codexHeaderFields returns the labeled fields among rows. A field starts
// after a blank row and rejoins its wrapped rows with spaces; a block without
// a known label is skipped.
func codexHeaderFields(rows []string) map[string]string {
	fields := make(map[string]string)
	label := ""
	for i, line := range rows {
		row := strings.TrimSpace(line)
		switch {
		case row == "":
			label = ""
		case i == 0 || strings.TrimSpace(rows[i-1]) == "":
			label = ""
			for _, l := range codexHeaderLabels {
				if codexRowHasLabel(row, l) {
					label = l
					fields[l] = strings.TrimSpace(strings.TrimPrefix(row, l))
					break
				}
			}
		case label != "":
			fields[label] += " " + row
		}
	}
	return fields
}

// codexRequestText renders the request field, rows[0] being its labeled row.
// A command is shown as Codex drew it (codexDrawnRows), with its blank lines
// and indentation past the overlay's own, since a line break where Codex
// would have wrapped looks like the wrap. Other fields rejoin the rows Codex
// wrapped for a pane width columns wide (codexUnwrapRows); each
// "Destination:" row names one file.
func codexRequestText(rows []string, label string, width int) string {
	if label == "$" {
		return strings.TrimPrefix(strings.TrimPrefix(codexDrawnRows(rows), "$"), " ")
	}
	var entries []string
	start := 0
	for i := 1; i <= len(rows); i++ {
		if i < len(rows) && (label != "Destination:" || !codexRowHasLabel(strings.TrimSpace(rows[i]), label)) {
			continue
		}
		entries = append(entries, strings.TrimSpace(strings.TrimPrefix(codexUnwrapRows(rows[start:i], width), label)))
		start = i
	}
	return strings.Join(entries, "\n")
}

// codexDrawnRows returns overlay rows as Codex drew them, without the
// overlay's indent, one per line.
func codexDrawnRows(rows []string) string {
	inset := strings.Repeat(" ", codexOverlayInset)
	drawn := make([]string, len(rows))
	for i, line := range rows {
		drawn[i] = strings.TrimRight(strings.TrimPrefix(line, inset), " ")
	}
	return strings.Join(drawn, "\n")
}

// codexUnwrapRows returns the text Codex wrapped onto overlay rows for a pane
// width columns wide (0 when unknown): each row without the overlay's indent,
// joined to the row above by the break codexRowBreak reads between them.
func codexUnwrapRows(rows []string, width int) string {
	inset := strings.Repeat(" ", codexOverlayInset)
	var text strings.Builder
	prev := ""
	for i, line := range rows {
		row := strings.TrimRight(strings.TrimPrefix(line, inset), " ")
		if i > 0 {
			text.WriteString(codexRowBreak(prev, row, width-2*codexOverlayInset))
		}
		text.WriteString(row)
		prev = row
	}
	return text.String()
}

// codexRowBreak returns what joined row to prev, the row above it, in text
// Codex wrapped to wrap columns: "" where Codex cut a word too long for prev,
// " " where the next word would not have fit on prev, and "\n" for a line
// break. A break next to a blank row, before a row that starts with a space,
// or next to a row whose width gc does not count is a line break, as is any
// break when the width is unknown.
func codexRowBreak(prev, row string, wrap int) string {
	switch {
	case wrap <= 0 || prev == "" || row == "" || row[0] == ' ':
		return "\n"
	case len(prev) > wrap || !codexSingleColumn(prev) || !codexSingleColumn(row):
		return "\n"
	case len(prev) == wrap && !strings.Contains(strings.TrimLeft(prev, " "), " "):
		return ""
	}
	if word, _, _ := strings.Cut(row, " "); len(prev)+1+len(word) > wrap {
		return " "
	}
	return "\n"
}

// codexSingleColumn reports whether s is plain printable ASCII, each byte of
// which takes one column.
func codexSingleColumn(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// codexRowHasLabel reports whether the trimmed row opens a field labeled
// label: "Reason: …", "$ …", or the bare label.
func codexRowHasLabel(row, label string) bool {
	return row == label || strings.HasPrefix(row, label+" ")
}

// trimBlankTail returns end moved up past the blank rows above it.
func trimBlankTail(lines []string, end int) int {
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return end
}
