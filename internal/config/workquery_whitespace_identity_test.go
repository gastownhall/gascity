package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// argLoggingStandIn answers every call with an empty JSON array and appends its
// argv, one bracketed argument after another, to $ARGV_LOG.
const argLoggingStandIn = `#!/bin/sh
{ printf '%s' "$(basename "$0")"; for a in "$@"; do printf '[%s]' "$a"; done; printf '\n'; } >> "$ARGV_LOG"
printf '[]'
`

// whitespaceAssignee matches an assignee filter whose value is only whitespace.
var whitespaceAssignee = regexp.MustCompile(`\[--assignee=\s*\]`)

// TestWorkQuerySkipsAWhitespaceOnlyIdentity pins the identity loop to the
// identities the claim pipeline recognizes (ga-ylpbsy.3.8). A session identity
// that is only whitespace is no identity (hookClaimAssigneeIdentity and the
// adoption candidates trim it to nothing), but the loop skipped only an EMPTY
// one, so it asked `gc ready --assignee="  "`. gc ready trims the flag and reads
// an empty --assignee as no filter, so that tier answered with anyone's work;
// the claim then refused it, and the query had already stopped at that tier.
func TestWorkQuerySkipsAWhitespaceOnlyIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		topo QueryTopology
	}{
		{name: "federated", topo: federatedTopology()},
		{name: "single store", topo: singleStoreTopology()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			for _, bin := range []string{"gc", "bd"} {
				writeFakeBin(t, filepath.Join(tmp, bin), argLoggingStandIn)
			}
			argvLog := filepath.Join(tmp, "argv.log")
			a := &Agent{Name: "worker"}
			env := []string{
				"PATH=" + tmp + ":" + os.Getenv("PATH"),
				"ARGV_LOG=" + argvLog,
				"GC_SESSION_ID=  ",
				"GC_SESSION_NAME=worker-1",
				"GC_ALIAS=\t",
				"GC_SESSION_ORIGIN=ephemeral",
			}
			_, stderr, exit := runShellCommandCapture(t, a.EffectiveWorkQueryFor(tc.topo), env)
			if exit != 0 {
				t.Fatalf("work query exit %d; stderr: %s", exit, stderr)
			}
			logged, err := os.ReadFile(argvLog)
			if err != nil {
				t.Fatalf("read argv log: %v", err)
			}
			if bad := whitespaceAssignee.FindString(string(logged)); bad != "" {
				t.Fatalf("a tier was asked for a whitespace-only assignee %q:\n%s", bad, logged)
			}
			if !strings.Contains(string(logged), "[--assignee=worker-1]") {
				t.Fatalf("the real identity was never queried:\n%s", logged)
			}
		})
	}
}
