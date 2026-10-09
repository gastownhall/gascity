package main

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const promptGCCommandsCheckName = "prompt-gc-commands"

// Kinds of prompt-gc-commands findings, as they appear in Details and in the
// JSON payload.
const (
	promptGCUnknownCommand    = "unknown-command"
	promptGCUnknownSubcommand = "unknown-subcommand"
	promptGCUnknownFlag       = "unknown-flag"
	promptGCDeprecatedCommand = "deprecated-command"
	promptGCDeprecatedFlag    = "deprecated-flag"
)

// promptGCCommandsDoctorCheck renders every agent's prompt and resolves each
// gc invocation the prompt writes as code (a fenced block line or an inline
// code span) against this gc's command tree plus the city's pack commands.
// It reports invocations naming a command, subcommand, or flag this gc does
// not have, so a prompt written for a different gc or pack version is caught
// before an agent follows it. Prose outside code is read only to skip an
// inline code span that follows a prohibition in its sentence ("Do not invent
// `gc mail list`").
//
// The check is advisory: the prohibition cues are a fixed word list, so a
// prompt can still quote a command it forbids in a way the list misses.
type promptGCCommandsDoctorCheck struct {
	cityPath string
	cfg      *config.City
	// root is the command tree of the running gc. A fresh tree is never built
	// here: constructing one rebinds the persistent-flag globals (--city,
	// --rig) the running command already parsed.
	root *cobra.Command
}

func newPromptGCCommandsDoctorCheck(cityPath string, cfg *config.City, root *cobra.Command) *promptGCCommandsDoctorCheck {
	return &promptGCCommandsDoctorCheck{cityPath: cityPath, cfg: cfg, root: root}
}

// Name implements doctor.Check.
func (*promptGCCommandsDoctorCheck) Name() string { return promptGCCommandsCheckName }

// CanFix implements doctor.Check. The fix is a prompt or pin edit the user
// owns.
func (*promptGCCommandsDoctorCheck) CanFix() bool { return false }

// Fix implements doctor.Check.
func (*promptGCCommandsDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

// WarmupEligible implements doctor.Check. Rendering prompts needs the full
// city configuration, which the gc start warm-up scan runs before.
func (*promptGCCommandsDoctorCheck) WarmupEligible() bool { return false }

// promptGCCommandsPayload is the --json payload of prompt-gc-commands.
type promptGCCommandsPayload struct {
	AgentsScanned int                       `json:"agents_scanned"`
	Invocations   int                       `json:"invocations"`
	Findings      []promptGCCommandFindings `json:"findings"`
}

// promptGCCommandFindings is one distinct unresolved invocation and every
// agent whose prompt writes it.
type promptGCCommandFindings struct {
	Kind    string   `json:"kind"`
	Command string   `json:"command"`
	Reason  string   `json:"reason"`
	Agents  []string `json:"agents"`
}

// Run implements doctor.Check.
func (c *promptGCCommandsDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: promptGCCommandsCheckName, Severity: doctor.SeverityAdvisory}
	if c.cfg == nil || c.root == nil {
		r.Status = doctor.StatusOK
		r.Message = "no city config or command tree to check against"
		return r
	}

	cityName := loadedCityName(c.cfg, c.cityPath)
	packRoot := &cobra.Command{Use: "gc"}
	addDiscoveredCommandsToRoot(packRoot, c.cfg.PackCommands, c.cityPath, cityName, io.Discard, io.Discard, false)
	resolver := promptGCResolver{roots: []*cobra.Command{c.root, packRoot}}

	agents := make([]config.Agent, len(c.cfg.Agents))
	copy(agents, c.cfg.Agents)
	sort.Slice(agents, func(i, j int) bool { return agents[i].QualifiedName() < agents[j].QualifiedName() })

	topo := cityQueryTopology(c.cityPath, c.cfg)
	byKey := make(map[string]*promptGCCommandFindings)
	payload := promptGCCommandsPayload{}
	for i := range agents {
		a := agents[i]
		if a.PromptTemplate == "" {
			continue
		}
		prompt, _ := renderDoctorAgentPrompt(c.cityPath, cityName, c.cfg, topo, &a)
		payload.AgentsScanned++
		for _, inv := range promptGCInvocations(prompt) {
			payload.Invocations++
			for _, f := range resolver.resolve(inv) {
				key := f.Kind + "\x00" + f.Command
				entry, ok := byKey[key]
				if !ok {
					entry = &promptGCCommandFindings{Kind: f.Kind, Command: f.Command, Reason: f.Reason}
					byKey[key] = entry
				}
				if n := len(entry.Agents); n == 0 || entry.Agents[n-1] != a.QualifiedName() {
					entry.Agents = append(entry.Agents, a.QualifiedName())
				}
			}
		}
	}

	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	payload.Findings = make([]promptGCCommandFindings, 0, len(keys))
	for _, k := range keys {
		f := *byKey[k]
		payload.Findings = append(payload.Findings, f)
		r.Details = append(r.Details, fmt.Sprintf("%s | %s | %s | agents: %s", f.Kind, f.Command, f.Reason, strings.Join(f.Agents, ", ")))
	}
	r.Payload = payload

	if len(payload.Findings) == 0 {
		r.Status = doctor.StatusOK
		r.Message = fmt.Sprintf("%d gc invocation(s) in %d agent prompt(s) resolve", payload.Invocations, payload.AgentsScanned)
		return r
	}
	r.Status = doctor.StatusWarning
	r.Message = fmt.Sprintf("%d gc invocation(s) in agent prompts do not resolve in this city", len(payload.Findings))
	r.FixHint = "update the prompt or fragment to a command this gc has, or pin the pack version whose prompts match this gc; run \"gc <command> --help\" to confirm"
	return r
}

// promptGCFinding is one unresolved part of one invocation.
type promptGCFinding struct {
	Kind    string
	Command string
	Reason  string
}

// promptGCResolver resolves gc invocations against one or more command
// roots. The first root is the gc command tree; later roots supply pack
// command namespaces the first does not already hold.
type promptGCResolver struct {
	roots []*cobra.Command
}

var (
	promptGCCommandWord = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	promptGCFlagName    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)
)

// resolve walks args (the words after "gc") the way cobra would: command
// words descend into subcommands, flags are looked up on the command reached
// so far (local, then persistent flags of it and its ancestors), and a value
// flag written without "=" consumes the next word. The walk stops at the
// first word that is neither a command word nor a flag (a placeholder, a
// quoted value, a path), and at any command that passes its arguments
// through unparsed.
func (r promptGCResolver) resolve(args []string) []promptGCFinding {
	if len(r.roots) == 0 {
		return nil
	}
	var findings []promptGCFinding
	var cur *cobra.Command
	path := []string{"gc"}
	positional := false
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if tok == "--" {
			break
		}
		if isPromptGCFlagToken(tok) {
			if cur != nil && cur.DisableFlagParsing {
				break
			}
			lookupIn := cur
			if lookupIn == nil {
				lookupIn = r.roots[0]
			}
			name, short, inlineValue := splitPromptGCFlag(tok)
			if !promptGCFlagName.MatchString(name) {
				break
			}
			if name == "help" || (short && name == "h") {
				continue
			}
			flag := lookupPromptGCFlag(lookupIn, name, short)
			display := strings.Join(path, " ") + " " + tok
			if flag == nil {
				findings = append(findings, promptGCFinding{
					Kind:    promptGCUnknownFlag,
					Command: display,
					Reason:  fmt.Sprintf("%q has no flag %q", strings.Join(path, " "), tok),
				})
				continue
			}
			if flag.Deprecated != "" {
				findings = append(findings, promptGCFinding{
					Kind:    promptGCDeprecatedFlag,
					Command: display,
					Reason:  fmt.Sprintf("flag is deprecated: %s", strings.TrimSpace(flag.Deprecated)),
				})
			}
			if !inlineValue && flag.NoOptDefVal == "" {
				i++
			}
			continue
		}
		if positional || !promptGCCommandWord.MatchString(tok) {
			if cur == nil {
				break
			}
			positional = true
			continue
		}
		next := r.child(cur, tok)
		if next == nil {
			switch {
			case cur == nil:
				findings = append(findings, promptGCFinding{
					Kind:    promptGCUnknownCommand,
					Command: "gc " + tok,
					Reason:  fmt.Sprintf("no gc command or pack command namespace named %q", tok),
				})
				return findings
			case acceptsNoPositionals(cur):
				findings = append(findings, promptGCFinding{
					Kind:    promptGCUnknownSubcommand,
					Command: strings.Join(path, " ") + " " + tok,
					Reason:  fmt.Sprintf("%q has no subcommand %q", strings.Join(path, " "), tok),
				})
				return findings
			}
			positional = true
			continue
		}
		cur = next
		path = append(path, tok)
		if cur.Deprecated != "" {
			findings = append(findings, promptGCFinding{
				Kind:    promptGCDeprecatedCommand,
				Command: strings.Join(path, " "),
				Reason:  fmt.Sprintf("command is deprecated: %s", strings.TrimSpace(cur.Deprecated)),
			})
		}
		if cur.DisableFlagParsing {
			break
		}
	}
	return findings
}

// child returns the subcommand of parent named word (by name or alias). A
// nil parent means the top level, where every root is consulted in order.
func (r promptGCResolver) child(parent *cobra.Command, word string) *cobra.Command {
	if parent != nil {
		return findPromptGCChild(parent, word)
	}
	if word == "help" || word == "completion" {
		// Cobra adds these to the root only when it executes.
		if c := findPromptGCChild(r.roots[0], word); c != nil {
			return c
		}
		return &cobra.Command{Use: word, DisableFlagParsing: true}
	}
	for _, root := range r.roots {
		if c := findPromptGCChild(root, word); c != nil {
			return c
		}
	}
	return nil
}

func findPromptGCChild(parent *cobra.Command, word string) *cobra.Command {
	for _, c := range parent.Commands() {
		if c.Name() == word || c.HasAlias(word) {
			return c
		}
	}
	return nil
}

// acceptsNoPositionals reports whether a word after cmd that is not one of
// its subcommands can only be a mistyped or missing subcommand: cmd is a
// command group whose usage line declares no arguments (cobra convention:
// "agent", not "hook [agent]").
func acceptsNoPositionals(cmd *cobra.Command) bool {
	return cmd.HasSubCommands() && len(strings.Fields(cmd.Use)) == 1
}

func isPromptGCFlagToken(tok string) bool {
	if len(tok) < 2 || tok[0] != '-' {
		return false
	}
	if tok[1] == '-' {
		return len(tok) > 2
	}
	c := tok[1]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// splitPromptGCFlag returns the flag name, whether it was written in
// shorthand, and whether its value is attached ("--x=v", "-xv").
func splitPromptGCFlag(tok string) (name string, short, inlineValue bool) {
	if strings.HasPrefix(tok, "--") {
		body := tok[2:]
		if eq := strings.IndexByte(body, '='); eq >= 0 {
			return body[:eq], false, true
		}
		return body, false, false
	}
	return tok[1:2], true, len(tok) > 2
}

// lookupPromptGCFlag finds a flag cobra would accept on cmd: its local and
// persistent flags, then the persistent flags of each ancestor. It reads the
// flag sets without merging them, so the live command tree is not mutated.
func lookupPromptGCFlag(cmd *cobra.Command, name string, short bool) *pflag.Flag {
	lookup := func(fs *pflag.FlagSet) *pflag.Flag {
		if short {
			return fs.ShorthandLookup(name)
		}
		return fs.Lookup(name)
	}
	if f := lookup(cmd.Flags()); f != nil {
		return f
	}
	for c := cmd; c != nil; c = c.Parent() {
		if f := lookup(c.PersistentFlags()); f != nil {
			return f
		}
	}
	return nil
}

// promptGCInvocations returns the words after "gc" for each gc invocation the
// prompt writes as code: any line inside a fenced block, or an inline code
// span outside one. An invocation starts at a "gc" word in command position
// (the start of the code, or after a shell operator, "$", or a command
// prefix such as "sudo") and runs to the next shell operator or comment.
//
// An inline code span that follows a prohibition cue in the same prose
// sentence ("do not", "never", "avoid", "instead of", ...; see
// promptGCProhibitionCue) is skipped: the prompt names that command to forbid
// it. Fenced code blocks are always read.
func promptGCInvocations(prompt string) [][]string {
	var out [][]string
	inFence := false
	fence := ""
	var sentence promptGCSentence
	for _, line := range strings.Split(prompt, "\n") {
		trimmed := strings.TrimSpace(line)
		if marker := promptFenceMarker(trimmed); marker != "" {
			switch {
			case !inFence:
				inFence, fence = true, marker
			case strings.HasPrefix(trimmed, fence) && strings.TrimSpace(strings.TrimLeft(trimmed, fence[:1])) == "":
				inFence = false
			}
			sentence.reset()
			continue
		}
		if inFence {
			out = append(out, gcInvocationsInShell(line)...)
			continue
		}
		if trimmed == "" {
			sentence.reset()
			continue
		}
		for _, span := range sentence.permittedCodeSpans(line) {
			out = append(out, gcInvocationsInShell(span)...)
		}
	}
	return out
}

// promptGCProhibitionCue matches prose that forbids what follows it in the
// sentence.
var promptGCProhibitionCue = regexp.MustCompile(`(?i)\b(do not|don['’]t|never|must not|should not|avoid|instead of|rather than)\b`)

// promptGCSentence tracks the prose of the sentence being read, which may
// span lines of one paragraph, and whether it has reached a prohibition cue.
type promptGCSentence struct {
	prose      strings.Builder
	forbidding bool
}

func (s *promptGCSentence) reset() {
	s.prose.Reset()
	s.forbidding = false
}

// note adds prose to the sentence. Sentence ends ('.', '!', '?' or ';'
// followed by a space or the end of the line) start a new sentence.
func (s *promptGCSentence) note(prose string) {
	for i := 0; i < len(prose); i++ {
		ch := prose[i]
		if (ch == '.' || ch == '!' || ch == '?' || ch == ';') && (i+1 == len(prose) || prose[i+1] == ' ' || prose[i+1] == '\t') {
			s.reset()
			continue
		}
		s.prose.WriteByte(ch)
	}
	if !s.forbidding && promptGCProhibitionCue.MatchString(s.prose.String()) {
		s.forbidding = true
	}
}

// permittedCodeSpans returns the contents of the backtick code spans on line
// that do not follow a prohibition cue in their sentence.
func (s *promptGCSentence) permittedCodeSpans(line string) []string {
	var spans []string
	prev := 0
	for i := 0; i < len(line); {
		if line[i] != '`' {
			i++
			continue
		}
		open := i
		for i < len(line) && line[i] == '`' {
			i++
		}
		delim := line[open:i]
		end := strings.Index(line[i:], delim)
		if end < 0 {
			break
		}
		s.note(line[prev:open])
		if !s.forbidding {
			spans = append(spans, line[i:i+end])
		}
		i += end + len(delim)
		prev = i
	}
	s.note(line[prev:] + " ")
	return spans
}

// promptFenceMarker returns the run of backticks or tildes opening a fenced
// code block line, or "" when the line is not a fence.
func promptFenceMarker(trimmed string) string {
	for _, ch := range []string{"`", "~"} {
		n := 0
		for n < len(trimmed) && trimmed[n] == ch[0] {
			n++
		}
		if n >= 3 {
			return trimmed[:n]
		}
	}
	return ""
}

// gcInvocationsInShell splits a line of shell into words and operators and
// returns the arguments of each gc invocation in command position.
func gcInvocationsInShell(line string) [][]string {
	toks := shellWords(line)
	var out [][]string
	for i := 0; i < len(toks); i++ {
		if toks[i].text != "gc" || toks[i].quoted {
			continue
		}
		if i > 0 && !isCommandPositionBefore(toks[i-1]) {
			continue
		}
		var args []string
		for j := i + 1; j < len(toks); j++ {
			if toks[j].operator || (!toks[j].quoted && strings.HasPrefix(toks[j].text, "#")) {
				break
			}
			if toks[j].quoted {
				args = append(args, "\x00quoted")
				continue
			}
			args = append(args, toks[j].text)
		}
		out = append(out, args)
	}
	return out
}

func isCommandPositionBefore(prev shellWord) bool {
	if prev.operator {
		return true
	}
	if prev.quoted {
		return false
	}
	switch prev.text {
	case "$", "sudo", "then", "do", "else", "exec", "time", "!", "env", "command":
		return true
	}
	return false
}

type shellWord struct {
	text     string
	quoted   bool
	operator bool
}

// shellWords splits line into words and the operators | & ; ( ) < > and
// "$(", honoring single and double quotes. A word containing a quote is
// marked quoted so it never reads as a command or flag name.
func shellWords(line string) []shellWord {
	var out []shellWord
	var cur strings.Builder
	quoted := false
	var quote byte
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, shellWord{text: cur.String(), quoted: quoted})
		}
		cur.Reset()
		quoted = false
	}
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if quote != 0 {
			if ch == quote {
				quote = 0
			} else {
				cur.WriteByte(ch)
			}
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
			quoted = true
		case ' ', '\t':
			flush()
		case '|', '&', ';', '(', ')', '<', '>', '`':
			flush()
			out = append(out, shellWord{text: string(ch), operator: true})
		case '$':
			if i+1 < len(line) && line[i+1] == '(' {
				flush()
				out = append(out, shellWord{text: "$(", operator: true})
				i++
				continue
			}
			cur.WriteByte(ch)
		default:
			cur.WriteByte(ch)
		}
	}
	flush()
	return out
}
