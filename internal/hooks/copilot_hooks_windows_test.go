//go:build windows

package hooks

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// Installing Copilot's hooks on Windows writes a powershell twin for every
// bash command, so Copilot CLI never falls back to whatever bash PATH offers.
func TestInstallCopilotHooksWritesPowerShellTwinsOnWindows(t *testing.T) {
	fs := fsys.NewFake()
	if err := Install(fs, "/city", "/work", []string{"copilot"}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	var data []byte
	for path, content := range fs.Files {
		if strings.HasSuffix(strings.ReplaceAll(path, `\`, "/"), "/work/.github/hooks/gascity.json") {
			data = content
		}
	}
	if data == nil {
		t.Fatalf("Install wrote no Copilot hook file; files: %v", fakeFileNames(fs))
	}
	var doc struct {
		Hooks map[string][]struct {
			Bash       string `json:"bash"`
			PowerShell string `json:"powershell"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parsing installed hooks: %v\n%s", err, data)
	}
	n := 0
	for event, entries := range doc.Hooks {
		for _, e := range entries {
			n++
			if !strings.Contains(e.PowerShell, "'gc.exe'") {
				t.Errorf("%s hook %q has no powershell twin calling gc.exe (got %q)", event, e.Bash, e.PowerShell)
			}
		}
	}
	if n == 0 {
		t.Fatalf("installed Copilot hook file has no entries:\n%s", data)
	}
}

func fakeFileNames(fs *fsys.Fake) []string {
	var names []string
	for path := range fs.Files {
		names = append(names, path)
	}
	return names
}
