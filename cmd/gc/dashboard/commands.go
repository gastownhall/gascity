package dashboard

import (
	"fmt"
	"regexp"
	"strings"
)

// CommandMeta describes a command's properties for the dashboard.
type CommandMeta struct {
	Safe     bool   // Safe commands can run without user confirmation
	Confirm  bool   // Confirm commands require user confirmation before execution
	Desc     string // Short description shown in the command palette
	Category string // Groups commands in the palette UI
	Args     string // Placeholder hint for required arguments
	ArgType  string // What kind of options to show (rigs, agents, convoys, hooks)
	Binary   string // "gc", "bd", or "api" — how the command is executed
}

// AllowedCommands defines which commands can be executed from the dashboard.
// Commands not in this list are blocked for security.
//
// Key mapping from upstream gastown:
//   - gt status        → gc status
//   - gt agents list   → gc agent list
//   - gt convoy X      → gc convoy X
//   - gt mail X        → gc mail X
//   - gt rig X         → gc rig X
//   - gt sling         → gc sling
//   - bd list/show     → dashboard API (no subprocess fallback)
//   - gt polecat add   → gc agent start
//   - gt mayor attach  → gc agent attach
var AllowedCommands = map[string]CommandMeta{
	// === Read-only gc commands (always safe) ===
	"status":            {Safe: true, Desc: "Show city status", Category: "Status", Binary: "gc"},
	"status --json":     {Safe: true, Desc: "Show city status (JSON)", Category: "Status", Binary: "gc"},
	"agent list":        {Safe: true, Desc: "List active agents", Category: "Status", Binary: "gc"},
	"agent list --json": {Safe: true, Desc: "List agents (JSON)", Category: "Status", Binary: "gc"},
	"convoy list":       {Safe: true, Desc: "List convoys", Category: "Convoys", Binary: "gc"},
	"convoy show":       {Safe: true, Desc: "Show convoy details", Category: "Convoys", Args: "<convoy-id>", ArgType: "convoys", Binary: "gc"},
	"convoy status":     {Safe: true, Desc: "Show convoy status", Category: "Convoys", Args: "<convoy-id> --json", ArgType: "convoys", Binary: "gc"},
	"mail inbox":        {Safe: true, Desc: "Check inbox", Category: "Mail", Binary: "gc"},
	"mail check":        {Safe: true, Desc: "Check for new mail", Category: "Mail", Binary: "gc"},
	"mail peek":         {Safe: true, Desc: "Peek at message", Category: "Mail", Args: "<message-id>", Binary: "gc"},
	"rig list":          {Safe: true, Desc: "List rigs", Category: "Rigs", Binary: "gc"},
	"rig show":          {Safe: true, Desc: "Show rig details", Category: "Rigs", Args: "<rig-name>", ArgType: "rigs", Binary: "gc"},
	"doctor":            {Safe: true, Desc: "Health check", Category: "Diagnostics", Binary: "gc"},
	"hooks list":        {Safe: true, Desc: "List hooks", Category: "Hooks", Binary: "gc"},
	"events --json":     {Safe: true, Desc: "Show events (JSON)", Category: "Status", Binary: "gc"},

	// === Read-only bead commands (API-backed; never subprocess fallback) ===
	"list": {Safe: true, Desc: "List beads", Category: "Beads", Binary: "api"},
	"show": {Safe: true, Desc: "Show bead details", Category: "Beads", Args: "<bead-id>", Binary: "api"},

	// === Action commands (require confirmation) ===

	// Mail actions
	"mail send":      {Confirm: true, Desc: "Send message", Category: "Mail", Args: "<address> -s <subject> -m <message>", ArgType: "agents", Binary: "gc"},
	"mail mark-read": {Confirm: false, Desc: "Mark as read", Category: "Mail", Args: "<message-id>", ArgType: "messages", Binary: "gc"},
	"mail archive":   {Confirm: false, Desc: "Archive message", Category: "Mail", Args: "<message-id>", ArgType: "messages", Binary: "gc"},
	"mail reply":     {Confirm: true, Desc: "Reply to message", Category: "Mail", Args: "<message-id> -m <message>", ArgType: "messages", Binary: "gc"},

	// Convoy actions
	"convoy create":  {Confirm: true, Desc: "Create convoy", Category: "Convoys", Args: "<name>", Binary: "gc"},
	"convoy target":  {Confirm: true, Desc: "Set convoy target", Category: "Convoys", Args: "<convoy-id> <branch>", ArgType: "convoys", Binary: "gc"},
	"convoy refresh": {Confirm: false, Desc: "Refresh convoy", Category: "Convoys", Args: "<convoy-id>", ArgType: "convoys", Binary: "gc"},
	"convoy add":     {Confirm: true, Desc: "Add issue to convoy", Category: "Convoys", Args: "<convoy-id> <issue>", ArgType: "convoys", Binary: "gc"},

	// Rig actions
	"rig boot":  {Confirm: true, Desc: "Boot rig", Category: "Rigs", Args: "<rig-name>", ArgType: "rigs", Binary: "gc"},
	"rig start": {Confirm: true, Desc: "Start rig", Category: "Rigs", Args: "<rig-name>", ArgType: "rigs", Binary: "gc"},

	// Agent lifecycle
	"agent start":  {Confirm: true, Desc: "Start agent", Category: "Agents", Args: "<agent-name>", ArgType: "agents", Binary: "gc"},
	"agent attach": {Confirm: true, Desc: "Attach to agent", Category: "Agents", Args: "<agent-name>", ArgType: "agents", Binary: "gc"},

	// Work assignment
	"sling":       {Confirm: true, Desc: "Assign work to agent", Category: "Work", Args: "<bead> <rig>", ArgType: "hooks", Binary: "gc"},
	"unsling":     {Confirm: true, Desc: "Unassign work from agent", Category: "Work", Args: "<bead>", ArgType: "hooks", Binary: "gc"},
	"hook attach": {Confirm: true, Desc: "Attach hook", Category: "Hooks", Args: "<bead>", ArgType: "hooks", Binary: "gc"},
	"hook detach": {Confirm: true, Desc: "Detach hook", Category: "Hooks", Args: "<bead>", ArgType: "hooks", Binary: "gc"},
}

// BlockedPatterns are regex patterns for commands that should never run from the dashboard.
var BlockedPatterns = []*regexp.Regexp{
	regexp.MustCompile(`--hard`),
	regexp.MustCompile(`\brm\b`),
	regexp.MustCompile(`\bdelete\b`),
	regexp.MustCompile(`\bkill\b`),
	regexp.MustCompile(`\bdestroy\b`),
	regexp.MustCompile(`\bpurge\b`),
	regexp.MustCompile(`\breset\b`),
	regexp.MustCompile(`\bclean\b`),
}

// forceFlagPattern matches --force as a standalone flag (with word
// boundary) so it doesn't misfire on --force-with-lease and friends.
var forceFlagPattern = regexp.MustCompile(`--force\b`)

// ForceAllowedCommands is the set of base commands permitted to carry
// `--force` through the dashboard gateway. Any other command that passes
// `--force` is rejected up front — a whitelisted command can still be
// dangerous when combined with --force, so we keep this check narrow and
// explicit rather than relying on command authors to remember which flags
// are safe. Add a new entry only after reviewing what --force does for
// that specific command.
var ForceAllowedCommands = map[string]bool{
	"sling": true,
}

// ValidateCommand checks if a command is allowed to run from the dashboard.
func ValidateCommand(rawCommand string) (*CommandMeta, error) {
	rawCommand = strings.TrimSpace(rawCommand)
	if rawCommand == "" {
		return nil, fmt.Errorf("empty command")
	}

	for _, pattern := range BlockedPatterns {
		if pattern.MatchString(rawCommand) {
			return nil, fmt.Errorf("command contains blocked pattern: %s", pattern.String())
		}
	}

	baseCmd := extractBaseCommand(rawCommand)
	meta, ok := AllowedCommands[baseCmd]
	if !ok {
		return nil, fmt.Errorf("command not in whitelist: %s", baseCmd)
	}

	// Defense-in-depth: --force bypasses singleton checks and idempotency
	// short-circuits. Only specific commands have a well-understood --force
	// semantics worth exposing through the dashboard; everything else gets
	// rejected even if the base command is whitelisted.
	if forceFlagPattern.MatchString(rawCommand) && !ForceAllowedCommands[baseCmd] {
		return nil, fmt.Errorf("--force is not permitted for command %q from the dashboard", baseCmd)
	}

	return &meta, nil
}

// extractBaseCommand gets the command prefix for whitelist matching.
func extractBaseCommand(cmd string) string {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return ""
	}

	// Try three-word command first (e.g., "agent list --json")
	if len(parts) >= 3 {
		threeWord := parts[0] + " " + parts[1] + " " + parts[2]
		if _, ok := AllowedCommands[threeWord]; ok {
			return threeWord
		}
	}

	// Try two-word command (e.g., "convoy list")
	if len(parts) >= 2 {
		twoWord := parts[0] + " " + parts[1]
		if _, ok := AllowedCommands[twoWord]; ok {
			return twoWord
		}
	}

	return parts[0]
}

// SanitizeArgs removes potentially dangerous characters from command arguments.
func SanitizeArgs(args []string) []string {
	sanitized := make([]string, 0, len(args))
	for _, arg := range args {
		clean := strings.Map(func(r rune) rune {
			switch r {
			case ';', '|', '&', '$', '`', '(', ')', '{', '}', '<', '>', '\n', '\r':
				return -1
			default:
				return r
			}
		}, arg)
		if clean != "" {
			sanitized = append(sanitized, clean)
		}
	}
	return sanitized
}

// GetCommandList returns all allowed commands for the command palette UI.
func GetCommandList() []CommandInfo {
	commands := make([]CommandInfo, 0, len(AllowedCommands))
	for name, meta := range AllowedCommands {
		commands = append(commands, CommandInfo{
			Name:     name,
			Desc:     meta.Desc,
			Category: meta.Category,
			Safe:     meta.Safe,
			Confirm:  meta.Confirm,
			Args:     meta.Args,
			ArgType:  meta.ArgType,
			Binary:   meta.Binary,
		})
	}
	return commands
}

// CommandInfo is the JSON-serializable form of a command for the UI.
type CommandInfo struct {
	Name     string `json:"name"`
	Desc     string `json:"desc"`
	Category string `json:"category"`
	Safe     bool   `json:"safe"`
	Confirm  bool   `json:"confirm"`
	Args     string `json:"args,omitempty"`
	ArgType  string `json:"argType,omitempty"`
	Binary   string `json:"binary"`
}
