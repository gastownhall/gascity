package builtinpacks

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// proxyRefusedCommand is a command that Beads v1.3.0-rc.1 rejects before it
// can reach the proxied storage provider. Keep this list deliberately small:
// it describes the front-door commands the Gas City packs must not invoke on
// the default path, not every command that happens to have a direct-only
// implementation in Beads.
var (
	proxyRefusedCommand = regexp.MustCompile(`(?:^|[[:space:]"'` + "`" + `();|])(?:gc[[:space:]]+)?bd[[:space:]]+(doctor|backup|rename-prefix)\b`)
	proxyRefusedWatch   = regexp.MustCompile(`(?:^|[[:space:]"'` + "`" + `();|])(?:gc[[:space:]]+)?bd[[:space:]]+show\b[^\r\n]*--watch\b`)
)

// proxyCapabilityExceptions is an expiring-by-resolution ledger for known
// pack gaps. An exception must name the issue that owns the fix; this prevents
// a newly introduced refusal from being hidden by a broad allow-list.
var proxyCapabilityExceptions = map[string]string{
	"gastown/agents/mayor/prompt.template.md:rename-prefix": "ga-p9iuv.4",
}

func TestActivePacksDoNotIntroduceUntrackedProxyRefusals(t *testing.T) {
	var findings []string
	for _, pack := range All() {
		pack := pack
		err := fs.WalkDir(pack.FS, ".", func(name string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			data, err := fs.ReadFile(pack.FS, name)
			if err != nil {
				return err
			}
			for lineNo, line := range strings.Split(string(data), "\n") {
				trimmed := strings.TrimSpace(line)
				// Comments and prose are not executed by a pack. In particular,
				// the reaper documents the old backup command in a shell comment.
				if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "<!--") {
					continue
				}
				command := ""
				switch {
				case proxyRefusedWatch.MatchString(line):
					command = "show --watch"
				case proxyRefusedCommand.MatchString(line):
					match := proxyRefusedCommand.FindStringSubmatch(line)
					command = match[1]
				}
				if command == "" {
					continue
				}
				key := path.Join(pack.Name, name) + ":" + command
				if issue, ok := proxyCapabilityExceptions[key]; ok {
					t.Logf("known proxied capability gap %s:%d (%s), tracked by %s", key, lineNo+1, strings.TrimSpace(line), issue)
					continue
				}
				findings = append(findings, fmt.Sprintf("%s:%d invokes refused proxied command %q: %s", key, lineNo+1, command, trimmed))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking active %s pack: %v", pack.Name, err)
		}
	}
	sort.Strings(findings)
	if len(findings) != 0 {
		t.Fatalf("active packs invoke commands refused by Beads proxied-server mode:\n%s\nAdd a narrowly-scoped issue-backed exception only while the owning fix is in flight", strings.Join(findings, "\n"))
	}
}

func TestProxyCapabilityExceptionKeysNameBundledPacks(t *testing.T) {
	for key, issue := range proxyCapabilityExceptions {
		parts := strings.SplitN(key, ":", 2)
		packParts := strings.SplitN(parts[0], "/", 2)
		if len(parts) != 2 || len(packParts) != 2 || packParts[0] == "" || packParts[1] == "" || parts[1] == "" {
			t.Errorf("exception key %q is not pack/path:command", key)
		}
		if _, ok := ByName(packParts[0]); !ok {
			t.Errorf("exception %q names unknown bundled pack", key)
		}
		if !strings.HasPrefix(issue, "ga-") {
			t.Errorf("exception %q issue %q is not a Gas City bead", key, issue)
		}
	}
}
