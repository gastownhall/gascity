package hooks

// Released managed hook documents that Install rewrites in place.
//
// Each constant is the exact bytes a release tag shipped, frozen on purpose. A
// shape must never be derived from the embedded overlay: the overlay moves with
// every hook change, and a derived shape stops matching the files already on
// disk the moment the overlay changes, which silently ends the upgrade it
// existed for. A new hook change adds the document it replaces here and leaves
// the existing shapes alone.
//
// Only released shapes belong here. Each one widens the set of on-disk files
// Install replaces as managed content, so a shape no workspace can be holding
// costs safety and buys nothing. Documents compare through
// overlay.CanonicalJSON, so formatting never decides a match.
//
// The Cursor documents released before v1.3.0 (v1.0.0, v1.1.0 through v1.2.1)
// differ from today's in the command body, and adopting them is a deliberate
// widening, so they stay un-upgraded.

const (
	// v1.3.0-rc1 through v1.4.2 (sha256 7e6bcc9d51e2174bcee84f432e4e21639cc145ff5f290999c3fa886928cdf883).
	releasedCursorHooksV130 = `{
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
	// v1.5.0-rc1 through v1.5.1 (sha256 7937ab9098ba65b3fd42b80d5f97cfc409b916ec71e3b97dab28d7a19d516673).
	releasedCursorHooksV150 = `{
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
	// v1.0.0-rc1 through v1.0.0 (sha256 cf92c52cd8bebb3fe08f4ad8c00db01a492ed158bcb0730a3c500425634b87e4).
	releasedCopilotHooksV100 = `{
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
	// v1.1.0 (sha256 e73cbaf8daeab547bd7efd2d486832ad6cd9df537d356c3f39ab3c071694a3ad).
	releasedCopilotHooksV110 = `{
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
	// v1.2.0-rc1 through v1.2.1 (sha256 df15c55f018fc8eeba17a0cbbfba05e97bb536143d55fa2beb7843956986869c).
	releasedCopilotHooksV120 = `{
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
	// v1.3.0-rc1 through v1.5.0 (sha256 72750bb7685ff1348b4ccfc7460aeeb2df5fb4e91cf7c4503d98bb82e71b4597).
	releasedCopilotHooksV130 = `{
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
	// v1.5.1-rc1 through v1.5.1 (sha256 09f65f8bb472b9796a14c381cbcf4231eb029799cd9184459c12cc8fab5e0c70).
	releasedCopilotHooksV151 = `{
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

// releasedCursorHookShapes lists every released .cursor/hooks.json Install
// replaces with the current overlay.
var releasedCursorHookShapes = []string{releasedCursorHooksV130, releasedCursorHooksV150}

// releasedCopilotHookShapes lists every released .github/hooks/gascity.json
// Install replaces with the current overlay.
var releasedCopilotHookShapes = []string{
	releasedCopilotHooksV100,
	releasedCopilotHooksV110,
	releasedCopilotHooksV120,
	releasedCopilotHooksV130,
	releasedCopilotHooksV151,
}
