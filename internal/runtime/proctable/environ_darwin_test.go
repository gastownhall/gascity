//go:build darwin

package proctable

import (
	"strings"
	"testing"
)

// ProcessEnvValue recovers a darwin process's environment by splitting the ps
// command column on whitespace and folding every key=value token into a map.
// These cases pin that parse, which is the half of ProcessEnvValue a test can
// drive: the ps exec has no injection seam, and liveScanGuard refuses the live
// process table under go test (gastownhall/gascity#2839).
func TestParseInlineEnvFromPSCommandColumn(t *testing.T) {
	const key = "GC_ACP_CONTROL_SOCKET"
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{
			name:    "plain value",
			command: "node agent.js --serve GC_SESSION_ID=sid " + key + "=/tmp/gc/acp/a1b2.sock",
			want:    "/tmp/gc/acp/a1b2.sock",
		},
		{
			// Known darwin limitation, documented on ProcessEnvValue: ps
			// flattens command, arguments and environment into one
			// whitespace-separated line, so a socket path under a directory
			// with a space truncates and matches nothing. The caller then sees
			// the same thing it sees for an absent key.
			name: "value containing a space truncates",
			command: "node agent.js GC_SESSION_ID=sid " +
				key + "=/Users/dev/Application Support/gc/a1b2.sock",
			want: "/Users/dev/Application",
		},
		{
			// A key=value-shaped argument parses as an environment entry, but
			// ps prints the environment after the arguments and the fold is
			// last-wins, so the real entry stays authoritative.
			name: "real environment outranks a same-named argument",
			command: "node agent.js --resume " + key + "=/tmp/from-argv.sock " +
				"GC_SESSION_ID=sid " + key + "=/tmp/gc/acp/a1b2.sock",
			want: "/tmp/gc/acp/a1b2.sock",
		},
		{
			name:    "absent key",
			command: "node agent.js --serve GC_SESSION_ID=sid",
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := parseInlineEnv(strings.Fields(tt.command))
			if got := env[key]; got != tt.want {
				t.Fatalf("parseInlineEnv(%q)[%s] = %q, want %q", tt.command, key, got, tt.want)
			}
			if got := env["GC_SESSION_ID"]; got != "sid" {
				t.Fatalf("parseInlineEnv(%q)[GC_SESSION_ID] = %q, want sid", tt.command, got)
			}
		})
	}
}
