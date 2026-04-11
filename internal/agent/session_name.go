package agent

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"text/template"
)

// sessionData holds template variables for custom session naming.
type sessionData struct {
	City  string // workspace name
	Agent string // sanitized qualified name (/ → --)
	Dir   string // rig/dir component (empty for singletons)
	Name  string // bare agent name
}

// SessionNameFor returns the session name for a city agent.
// This is the single source of truth for the naming convention.
// sessionTemplate is a Go text/template string; empty means use the
// default pattern "{agent}" (the sanitized agent name). With per-city
// controller/provider scoping as the default, the city prefix is usually unnecessary.
//
// For rig-scoped agents (name contains "/"), the dir and name
// components are joined with "--" to avoid path separators in session names:
//
//	"mayor"               → "mayor"
//	"hello-world/polecat" → "hello-world--polecat"
func SessionNameFor(cityName, agentName, sessionTemplate string) string {
	// Pre-sanitize: replace "/" with "--" for session-name compatibility.
	sanitized := strings.ReplaceAll(agentName, "/", "--")

	if sessionTemplate == "" {
		// Default: just the sanitized agent name. City scoping is handled
		// outside the name itself, so a prefix is usually redundant.
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
