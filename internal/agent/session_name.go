package agent

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"text/template"
)

var sessionNameQualifiedReplacer = strings.NewReplacer(
	"/", "--",
	".", "__",
)

var sessionNameQualifiedReverseReplacer = strings.NewReplacer(
	"--", "/",
	"__", ".",
)

// sessionData holds template variables for custom session naming.
type sessionData struct {
	City  string // workspace name
	Agent string // tmux-safe qualified name (/ -> --, . -> __)
	Dir   string // rig/dir component (empty for singletons)
	Name  string // bare agent name
}

// SanitizeQualifiedNameForSession converts a qualified identity into the
// deterministic tmux-safe form used by runtime session_name values.
func SanitizeQualifiedNameForSession(agentName string) string {
	return sessionNameQualifiedReplacer.Replace(agentName)
}

// UnsanitizeQualifiedNameFromSession best-effort decodes a tmux-safe runtime
// session name fragment back to the corresponding qualified identity.
func UnsanitizeQualifiedNameFromSession(name string) string {
	return sessionNameQualifiedReverseReplacer.Replace(name)
}

// SessionNameFor returns the session name for a city agent.
// This is the single source of truth for the naming convention.
// sessionTemplate is a Go text/template string; empty means use the
// default pattern "{agent}" (the sanitized agent name). With per-city
// tmux socket isolation as the default, the city prefix is unnecessary.
//
// For qualified identities, structural separators are encoded to avoid tmux
// naming issues while preserving slash-vs-dot distinction:
//
//	"mayor"               → "mayor"
//	"hello-world/polecat" → "hello-world--polecat"
//	"gastown.mayor"       → "gastown__mayor"
func SessionNameFor(cityName, agentName, sessionTemplate string) string {
	sanitized := SanitizeQualifiedNameForSession(agentName)

	if sessionTemplate == "" {
		// Default: just the sanitized agent name. Per-city tmux socket
		// isolation makes a city prefix redundant.
		return sanitized
	}

	// Parse dir/name components for template variables.
	var dir, name string
	if i := strings.LastIndex(agentName, "/"); i >= 0 {
		dir = agentName[:i]
		name = agentName[i+1:]
	} else {
		name = agentName
	}

	tmpl, err := template.New("session").Parse(sessionTemplate)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gc: session_template parse error: %v (using default)\n", err)
		return sanitized
	}

	var buf bytes.Buffer
	data := sessionData{
		City:  cityName,
		Agent: sanitized,
		Dir:   dir,
		Name:  name,
	}
	if err := tmpl.Execute(&buf, data); err != nil {
		fmt.Fprintf(os.Stderr, "gc: session_template execute error: %v (using default)\n", err)
		return sanitized
	}
	return buf.String()
}
