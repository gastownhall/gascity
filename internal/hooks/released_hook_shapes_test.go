package hooks

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bootstrap/packs/core"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/overlay"
)

// releasedHookFixture is a managed hook document exactly as a release tag
// shipped it. adopted says whether Install replaces it with the current
// overlay; the Cursor documents released before v1.3.0 are deliberately left
// alone.
type releasedHookFixture struct {
	name     string
	provider string
	sha256   string
	adopted  bool
	content  string
}

// overlayRel is the file's path under the provider overlay and the workdir.
func (f releasedHookFixture) overlayRel() string {
	if f.provider == "cursor" {
		return path.Join(".cursor", "hooks.json")
	}
	return path.Join(".github", "hooks", "gascity.json")
}

func (f releasedHookFixture) dst() string { return path.Join("/work", f.overlayRel()) }

// requireReleasedBytes fails unless the fixture is the tagged file.
func requireReleasedBytes(t *testing.T, f releasedHookFixture) {
	t.Helper()
	sum := sha256.Sum256([]byte(f.content))
	if got := hex.EncodeToString(sum[:]); got != f.sha256 {
		t.Fatalf("fixture %q is not the released file: sha256 %s, want %s", f.name, got, f.sha256)
	}
}

// currentOverlay returns what Install writes for a provider's hook file today.
func currentOverlay(t *testing.T, f releasedHookFixture) []byte {
	t.Helper()
	raw, err := core.PackFS.ReadFile(path.Join("overlay", "per-provider", f.provider, f.overlayRel()))
	if err != nil {
		t.Fatalf("read embedded %s overlay: %v", f.provider, err)
	}
	want, err := overlay.CanonicalJSON(raw)
	if err != nil {
		t.Fatalf("canonicalize embedded %s overlay: %v", f.provider, err)
	}
	return want
}

// TestInstallUpgradesReleasedDiscardedOutputHooks pins the rollout of the
// ga-m4xy49 removal: a workdir holding any released copilot or cursor hook file
// that runs nudge drain / mail check gets the current overlay, with the old
// bytes in a .bak. Install never rewrites an existing managed file unless it
// recognizes a released shape, so a shape it misses keeps draining nudges and
// archiving handoff mail into output the provider discards.
//
// The fixtures are the release tags' bytes, not derived from the overlay,
// because a fixture derived from the overlay moves with the change under test
// and cannot see an upgrade that stopped working.
func TestInstallUpgradesReleasedDiscardedOutputHooks(t *testing.T) {
	adopted := 0
	for _, f := range releasedHookFixtures {
		if !f.adopted {
			continue
		}
		adopted++
		t.Run(f.name, func(t *testing.T) {
			requireReleasedBytes(t, f)
			if !strings.Contains(f.content, "nudge drain") || !strings.Contains(f.content, "mail check") {
				t.Fatalf("fixture no longer carries the consumptive hooks; it needs no upgrade:\n%s", f.content)
			}
			want := currentOverlay(t, f)
			if got, err := overlay.CanonicalJSON([]byte(f.content)); err != nil || bytes.Equal(got, want) {
				t.Fatalf("fixture canonicalizes to the current overlay (err %v); an upgrade would rewrite identical content", err)
			}

			fs := fsys.NewFake()
			fs.Files[f.dst()] = []byte(f.content)
			if err := Install(fs, "/city", "/work", []string{f.provider}); err != nil {
				t.Fatalf("Install: %v", err)
			}
			got := fs.Files[f.dst()]
			if !bytes.Equal(got, want) {
				t.Fatalf("released %s hook file was not replaced by the current overlay:\n%s", f.provider, got)
			}
			for _, consumptive := range []string{"nudge drain", "mail check"} {
				if bytes.Contains(got, []byte(consumptive)) {
					t.Fatalf("upgraded %s hook file still runs %q:\n%s", f.provider, consumptive, got)
				}
			}
			if backup := fs.Files[f.dst()+".bak"]; !bytes.Equal(backup, []byte(f.content)) {
				t.Fatalf("backup = %q, want the released bytes", backup)
			}

			// A second Install finds the current overlay and changes nothing.
			if err := Install(fs, "/city", "/work", []string{f.provider}); err != nil {
				t.Fatalf("second Install: %v", err)
			}
			if !bytes.Equal(fs.Files[f.dst()], want) {
				t.Fatalf("second Install changed the upgraded file:\n%s", fs.Files[f.dst()])
			}
			if _, extra := fs.Files[f.dst()+".bak.1"]; extra {
				t.Fatal("second Install backed up the already-current file")
			}
		})
	}
	if adopted == 0 {
		t.Fatal("no released fixtures are adopted; this test has lost its subject")
	}
}

// TestInstallLeavesUnadoptedReleasedHooks pins the deliberate limit of the
// upgrade: Cursor documents released before v1.3.0 differ from today's in the
// command body, adopting them is a widening decision rather than a fix, and so
// Install leaves them alone.
func TestInstallLeavesUnadoptedReleasedHooks(t *testing.T) {
	left := 0
	for _, f := range releasedHookFixtures {
		if f.adopted {
			continue
		}
		left++
		t.Run(f.name, func(t *testing.T) {
			requireReleasedBytes(t, f)
			fs := fsys.NewFake()
			fs.Files[f.dst()] = []byte(f.content)
			if err := Install(fs, "/city", "/work", []string{f.provider}); err != nil {
				t.Fatalf("Install: %v", err)
			}
			if got := fs.Files[f.dst()]; string(got) != f.content {
				t.Fatalf("unadopted released file was rewritten:\n%s", got)
			}
		})
	}
	if left == 0 {
		t.Fatal("every released fixture is adopted; delete this test or restore the limit")
	}
}

// TestInstallPreservesEditedReleasedHooks guards the upgrade against clobbering
// a user's work: a released document the user has added to is not managed
// content any more, so Install leaves it, and its hooks, alone.
func TestInstallPreservesEditedReleasedHooks(t *testing.T) {
	for _, f := range releasedHookFixtures {
		if !f.adopted {
			continue
		}
		t.Run(f.name, func(t *testing.T) {
			requireReleasedBytes(t, f)
			edited := strings.Replace(f.content, "{", `{"userNote": "mine",`, 1)
			fs := fsys.NewFake()
			fs.Files[f.dst()] = []byte(edited)
			if err := Install(fs, "/city", "/work", []string{f.provider}); err != nil {
				t.Fatalf("Install: %v", err)
			}
			if got := fs.Files[f.dst()]; string(got) != edited {
				t.Fatalf("user-edited %s hook file was overwritten:\n%s", f.provider, got)
			}
			if _, ok := fs.Files[f.dst()+".bak"]; ok {
				t.Fatal("a user-edited file was backed up as if it were replaced")
			}
		})
	}
}

func TestInstallCopilotHookPreservesUserAuthoredFile(t *testing.T) {
	fs := fsys.NewFake()
	const dst = "/work/.github/hooks/gascity.json"
	custom := []byte(`{"version":1,"hooks":{"sessionStart":[{"type":"command","bash":"my-copilot-hook"}]}}`)
	fs.Files[dst] = custom

	if err := Install(fs, "/city", "/work", []string{"copilot"}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got := fs.Files[dst]; !bytes.Equal(got, custom) {
		t.Fatalf("user-authored Copilot hook was overwritten:\n%s", got)
	}
}

// Fixtures: the documents release tags shipped, byte for byte. fixture.sha256
// proves each literal is the tagged file, so a transcription or formatting slip
// fails loudly instead of testing the wrong document.
var releasedHookFixtures = []releasedHookFixture{
	{name: "cursor v1.0.0-rc1 through v1.0.0", provider: "cursor", sha256: "8c779de5278d4280695e4c540ecc7392edb373d3d782a64fba8d6a296f755049", adopted: false, content: fixtureCursorV100},
	{name: "cursor v1.1.0 through v1.2.1", provider: "cursor", sha256: "92fe04a24e555c7140a0270afa6525cafeaefd784fa7d2a7af4ae294d16737c2", adopted: false, content: fixtureCursorV110},
	{name: "cursor v1.3.0-rc1 through v1.4.2", provider: "cursor", sha256: "7e6bcc9d51e2174bcee84f432e4e21639cc145ff5f290999c3fa886928cdf883", adopted: true, content: fixtureCursorV130},
	{name: "cursor v1.5.0-rc1 through v1.5.1", provider: "cursor", sha256: "7937ab9098ba65b3fd42b80d5f97cfc409b916ec71e3b97dab28d7a19d516673", adopted: true, content: fixtureCursorV150},
	{name: "copilot v1.0.0-rc1 through v1.0.0", provider: "copilot", sha256: "cf92c52cd8bebb3fe08f4ad8c00db01a492ed158bcb0730a3c500425634b87e4", adopted: true, content: fixtureCopilotV100},
	{name: "copilot v1.1.0", provider: "copilot", sha256: "e73cbaf8daeab547bd7efd2d486832ad6cd9df537d356c3f39ab3c071694a3ad", adopted: true, content: fixtureCopilotV110},
	{name: "copilot v1.2.0-rc1 through v1.2.1", provider: "copilot", sha256: "df15c55f018fc8eeba17a0cbbfba05e97bb536143d55fa2beb7843956986869c", adopted: true, content: fixtureCopilotV120},
	{name: "copilot v1.3.0-rc1 through v1.5.0", provider: "copilot", sha256: "72750bb7685ff1348b4ccfc7460aeeb2df5fb4e91cf7c4503d98bb82e71b4597", adopted: true, content: fixtureCopilotV130},
	{name: "copilot v1.5.1-rc1 through v1.5.1", provider: "copilot", sha256: "09f65f8bb472b9796a14c381cbcf4231eb029799cd9184459c12cc8fab5e0c70", adopted: true, content: fixtureCopilotV151},
}

const (
	fixtureCursorV100 = `{
  "version": 1,
  "hooks": {
    "sessionStart": [
      {
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc prime --hook"
      }
    ],
    "preCompact": [
      {
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc handoff \"context cycle\""
      }
    ],
    "beforeSubmitPrompt": [
      {
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc nudge drain --inject"
      },
      {
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc mail check --inject"
      }
    ],
    "stop": [
      {
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc hook --inject"
      }
    ]
  }
}
`
	fixtureCursorV110 = `{
  "version": 1,
  "hooks": {
    "sessionStart": [
      {
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc prime --hook"
      }
    ],
    "preCompact": [
      {
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc handoff --auto \"context cycle\""
      }
    ],
    "beforeSubmitPrompt": [
      {
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc nudge drain --inject"
      },
      {
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc mail check --inject"
      }
    ]
  }
}
`
	fixtureCursorV130 = `{
  "version": 1,
  "hooks": {
    "sessionStart": [
      {
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc prime --hook"
      }
    ],
    "preCompact": [
      {
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc handoff --auto \"context cycle\""
      }
    ],
    "beforeSubmitPrompt": [
      {
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc hook run --timeout 15s --timeout-exit-code 0 -- nudge drain --inject"
      },
      {
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc hook run --timeout 15s --timeout-exit-code 0 -- mail check --inject"
      }
    ]
  }
}
`
	fixtureCursorV150 = `{
  "version": 1,
  "hooks": {
    "sessionStart": [
      {
        "command": "export PATH=\"$PATH:$HOME/go/bin:$HOME/.local/bin\"; if [ -n \"${BD_BIN:-}\" ]; then export PATH=\"${BD_BIN%/*}:$PATH\"; fi; \"${GC_BIN:-gc}\" prime --hook"
      }
    ],
    "preCompact": [
      {
        "command": "export PATH=\"$PATH:$HOME/go/bin:$HOME/.local/bin\"; if [ -n \"${BD_BIN:-}\" ]; then export PATH=\"${BD_BIN%/*}:$PATH\"; fi; \"${GC_BIN:-gc}\" handoff --auto \"context cycle\""
      }
    ],
    "beforeSubmitPrompt": [
      {
        "command": "export PATH=\"$PATH:$HOME/go/bin:$HOME/.local/bin\"; if [ -n \"${BD_BIN:-}\" ]; then export PATH=\"${BD_BIN%/*}:$PATH\"; fi; \"${GC_BIN:-gc}\" hook run --timeout 15s --timeout-exit-code 0 -- nudge drain --inject"
      },
      {
        "command": "export PATH=\"$PATH:$HOME/go/bin:$HOME/.local/bin\"; if [ -n \"${BD_BIN:-}\" ]; then export PATH=\"${BD_BIN%/*}:$PATH\"; fi; \"${GC_BIN:-gc}\" hook run --timeout 15s --timeout-exit-code 0 -- mail check --inject"
      }
    ]
  }
}
`
	fixtureCopilotV100 = `{
  "version": 1,
  "hooks": {
    "sessionStart": [
      {
        "type": "command",
        "bash": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc prime --hook",
        "timeoutSec": 30
      }
    ],
    "userPromptSubmitted": [
      {
        "type": "command",
        "bash": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc nudge drain --inject",
        "timeoutSec": 10
      },
      {
        "type": "command",
        "bash": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc mail check --inject",
        "timeoutSec": 10
      }
    ],
    "sessionEnd": [
      {
        "type": "command",
        "bash": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc hook --inject",
        "timeoutSec": 10
      }
    ]
  }
}
`
	fixtureCopilotV110 = `{
  "version": 1,
  "hooks": {
    "sessionStart": [
      {
        "type": "command",
        "bash": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc prime --hook",
        "timeoutSec": 30
      }
    ],
    "userPromptSubmitted": [
      {
        "type": "command",
        "bash": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc nudge drain --inject",
        "timeoutSec": 10
      },
      {
        "type": "command",
        "bash": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc mail check --inject",
        "timeoutSec": 10
      }
    ]
  }
}
`
	fixtureCopilotV120 = `{
  "version": 1,
  "hooks": {
    "sessionStart": [
      {
        "type": "command",
        "bash": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc prime --hook",
        "timeoutSec": 30
      }
    ],
    "preCompact": [
      {
        "type": "command",
        "bash": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc handoff --auto \"context cycle\"",
        "timeoutSec": 30
      }
    ],
    "userPromptSubmitted": [
      {
        "type": "command",
        "bash": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc nudge drain --inject",
        "timeoutSec": 10
      },
      {
        "type": "command",
        "bash": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc mail check --inject",
        "timeoutSec": 10
      }
    ]
  }
}
`
	fixtureCopilotV130 = `{
  "version": 1,
  "hooks": {
    "sessionStart": [
      {
        "type": "command",
        "bash": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc prime --hook",
        "timeoutSec": 30
      }
    ],
    "preCompact": [
      {
        "type": "command",
        "bash": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc handoff --auto \"context cycle\"",
        "timeoutSec": 30
      }
    ],
    "userPromptSubmitted": [
      {
        "type": "command",
        "bash": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc hook run --timeout 15s --timeout-exit-code 0 -- nudge drain --inject",
        "timeoutSec": 30
      },
      {
        "type": "command",
        "bash": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc hook run --timeout 15s --timeout-exit-code 0 -- mail check --inject",
        "timeoutSec": 30
      }
    ]
  }
}
`
	fixtureCopilotV151 = `{
  "version": 1,
  "hooks": {
    "sessionStart": [
      {
        "type": "command",
        "bash": "export PATH=\"$PATH:$HOME/go/bin:$HOME/.local/bin\" && \"${GC_BIN:-gc}\" prime --hook",
        "timeoutSec": 30
      }
    ],
    "preCompact": [
      {
        "type": "command",
        "bash": "export PATH=\"$PATH:$HOME/go/bin:$HOME/.local/bin\" && \"${GC_BIN:-gc}\" handoff --auto \"context cycle\"",
        "timeoutSec": 30
      }
    ],
    "userPromptSubmitted": [
      {
        "type": "command",
        "bash": "export PATH=\"$PATH:$HOME/go/bin:$HOME/.local/bin\" && \"${GC_BIN:-gc}\" hook run --timeout 15s --timeout-exit-code 0 -- nudge drain --inject",
        "timeoutSec": 30
      },
      {
        "type": "command",
        "bash": "export PATH=\"$PATH:$HOME/go/bin:$HOME/.local/bin\" && \"${GC_BIN:-gc}\" hook run --timeout 15s --timeout-exit-code 0 -- mail check --inject",
        "timeoutSec": 30
      }
    ]
  }
}
`
)
