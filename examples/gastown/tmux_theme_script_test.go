package gastown_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTmuxThemeScriptDrivesByScopeTier exercises tmux-theme.sh against a
// stubbed tmux that logs every set-option call. The script under test
// keys on session-name shape (no role awareness):
//
//	"<rig>--*"     -> rig tier   (color: ocean,  icon: ⛏)
//	"<scope>__*"   -> scope tier (color: plum,   icon: 🏛)
//	"<base>-N"     -> pool tier  (color: tan,    icon: 🌊)
//	anything else  -> default    (color: slate,  icon: ●)
//
// The cases below cover one example per tier plus regression coverage
// for the original bug: a crew agent (gascity--navani) used to fall to
// the default tier because the role-detection regex looked for a literal
// "crew" substring that the SDK session-name primitive never produces.
func TestTmuxThemeScriptDrivesByScopeTier(t *testing.T) {
	themeScript := filepath.Join(exampleDir(), "packs", "gastown", "assets", "scripts", "tmux-theme.sh")
	if _, err := os.Stat(themeScript); err != nil {
		t.Fatalf("tmux-theme.sh not found at %s: %v", themeScript, err)
	}

	tests := []struct {
		name    string
		session string
		agent   string
		// wantBG / wantIcon anchor the assertion. Empty wantBG means "skip color check".
		wantBG   string
		wantIcon string
	}{
		{
			name:     "rig tier: rig-scoped agent",
			session:  "gascity--witness",
			agent:    "gascity/witness",
			wantBG:   "#1e3a5f",
			wantIcon: "⛏",
		},
		{
			name:     "rig tier: crew (regression for original bug)",
			session:  "gascity--navani",
			agent:    "gascity/navani",
			wantBG:   "#1e3a5f",
			wantIcon: "⛏",
		},
		{
			name:     "rig tier: rig-scoped pool member (--polecat-1)",
			session:  "gascity--polecat-1",
			agent:    "gascity/polecat-1",
			wantBG:   "#1e3a5f",
			wantIcon: "⛏",
		},
		{
			name:     "scope tier: city-scoped role",
			session:  "gastown__mayor",
			agent:    "gastown.mayor",
			wantBG:   "#2d1f3d",
			wantIcon: "🏛",
		},
		{
			name:     "scope tier: another city-scoped role",
			session:  "gastown__deacon",
			agent:    "gastown.deacon",
			wantBG:   "#2d1f3d",
			wantIcon: "🏛",
		},
		{
			name:     "pool tier: generic <base>-N member",
			session:  "dog-1",
			agent:    "dog-1",
			wantBG:   "#3d2f1f",
			wantIcon: "🌊",
		},
		{
			name:     "default tier: bare name (no separator)",
			session:  "boot",
			agent:    "boot",
			wantBG:   "#4a5568",
			wantIcon: "●",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binDir := t.TempDir()
			optLog := filepath.Join(t.TempDir(), "tmux-set-option.log")

			// Stub tmux: log every argv to the file. The script only invokes
			// `tmux set-option ...` after the tier branches, so the log
			// captures the tier-driven decisions.
			writeExecutable(t, filepath.Join(binDir, "tmux"), `#!/bin/sh
# Drop a leading "-L <socket>" pair if present (the script sets it via
# GC_TMUX_SOCKET when configured; we leave that env unset here, but be
# defensive in case the harness changes).
if [ "$1" = "-L" ]; then
    shift 2
fi
printf '%s\n' "$*" >> "$TMUX_OPT_LOG"
exit 0
`)

			env := map[string]string{
				"PATH":           binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
				"TMUX_OPT_LOG":   optLog,
				"GC_TMUX_SOCKET": "", // disable -L flag so stub sees clean argv
			}

			cmd := exec.Command(themeScript, tt.session, tt.agent, "/tmp/cfg")
			cmd.Env = mergeTestEnv(env)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("tmux-theme.sh failed: %v\n%s", err, out)
			}

			logBytes, _ := os.ReadFile(optLog)
			log := string(logBytes)

			// Color: search for the bg=<hex> in the status-style call.
			if tt.wantBG != "" {
				wantStyle := "status-style bg=" + tt.wantBG
				if !strings.Contains(log, wantStyle) {
					t.Errorf("expected status-style with bg=%s in log, got:\n%s", tt.wantBG, log)
				}
			}

			// Icon: search for the icon in the status-left call.
			if tt.wantIcon != "" {
				wantLeft := "status-left " + tt.wantIcon + " " + tt.agent
				if !strings.Contains(log, wantLeft) {
					t.Errorf("expected status-left containing %q for agent %q, got:\n%s", tt.wantIcon, tt.agent, log)
				}
			}
		})
	}
}

// TestTmuxThemeScriptHasNoHardcodedRoleNames is a structural property
// test: the new tier-based script must not contain any hardcoded role
// names from the gastown taxonomy. The whole point of the fix is
// pack-portability — a custom pack with different roles should be
// themed correctly without forking this script.
func TestTmuxThemeScriptHasNoHardcodedRoleNames(t *testing.T) {
	themeScript := filepath.Join(exampleDir(), "packs", "gastown", "assets", "scripts", "tmux-theme.sh")
	body, err := os.ReadFile(themeScript)
	if err != nil {
		t.Fatalf("read tmux-theme.sh: %v", err)
	}
	src := string(body)

	// Strip leading-comment lines so we don't false-positive on docs that
	// mention old role names (e.g. "(witness, refinery, polecat, crew within
	// a rig)" in the tier-mapping comment).
	var codeOnly strings.Builder
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		codeOnly.WriteString(line)
		codeOnly.WriteString("\n")
	}
	code := codeOnly.String()

	forbidden := []string{
		"polecat", "witness", "refinery", "crew",
		"mayor", "deacon", "boot", "dog",
	}
	for _, role := range forbidden {
		if strings.Contains(code, role) {
			t.Errorf("script code (excluding comments) contains hardcoded role name %q; "+
				"the tier-based fix removes role-name awareness", role)
		}
	}
}
