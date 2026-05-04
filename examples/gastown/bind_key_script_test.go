package gastown_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBindKeyScriptDirectBind exercises bind-key.sh against a stubbed
// tmux that controls list-keys output and logs bind-key invocations.
//
// The script under test installs a tmux prefix binding directly,
// without if-shell wrapping or fallback parsing. Per-city tmux socket
// isolation (GC_TMUX_SOCKET, set by the controller) guarantees every
// session on the socket is a GC session, so there is no non-GC
// fallback path to preserve.
//
// The test cases verify:
//
//  1. No existing binding: bind-key is called with the command directly
//     (no if-shell wrapper, no fallback).
//  2. Existing default tmux binding ("next-window"): bind-key called
//     with the GC command, overwriting cleanly. (Regression: the prior
//     shape would wrap the existing binding inside if-shell, leading
//     to recursive accumulation across re-runs.)
//  3. Already-bound to the same GC command: bind-key NOT called
//     (idempotency optimization — the command is already there).
//  4. Already-bound to a different GC command: bind-key called with
//     the new command (overwrite).
//
// Cases 2 + 3 between them rule out the recursive-wrapping bug: there
// is no way for the script to install if-shell, so re-runs cannot
// nest layers.
func TestBindKeyScriptDirectBind(t *testing.T) {
	bindKey := filepath.Join(exampleDir(), "packs", "gastown", "assets", "scripts", "bind-key.sh")
	if _, err := os.Stat(bindKey); err != nil {
		t.Fatalf("bind-key.sh not found at %s: %v", bindKey, err)
	}

	tests := []struct {
		name           string
		key            string
		command        string
		listKeysOutput string // simulated tmux list-keys output
		// wantBindKeyCalled = true means we expect bind-key to be invoked.
		// wantBindKeyArgs is checked as a substring of the logged invocation.
		wantBindKeyCalled bool
		wantBindKeyArgs   string
		// Also assert what we did NOT see (regression checks).
		wantNotInLog []string
	}{
		{
			name:              "no existing binding installs command directly",
			key:               "n",
			command:           "run-shell '/path/to/cycle.sh next #{session_name} #{client_tty}'",
			listKeysOutput:    "",
			wantBindKeyCalled: true,
			wantBindKeyArgs:   "bind-key -T prefix n",
			wantNotInLog:      []string{"if-shell", "show-environment"},
		},
		{
			name:              "default tmux binding overwritten without if-shell wrap",
			key:               "n",
			command:           "run-shell '/path/to/cycle.sh next #{session_name} #{client_tty}'",
			listKeysOutput:    "bind-key -T prefix n next-window\n",
			wantBindKeyCalled: true,
			wantBindKeyArgs:   "bind-key -T prefix n",
			// Regression: the prior shape would have wrapped "next-window"
			// inside if-shell as the fallback. We want no if-shell at all.
			wantNotInLog: []string{"if-shell", "next-window"},
		},
		{
			name:              "idempotent: same command already bound is a no-op",
			key:               "n",
			command:           "run-shell '/path/to/cycle.sh next #{session_name} #{client_tty}'",
			listKeysOutput:    "bind-key -T prefix n run-shell '/path/to/cycle.sh next #{session_name} #{client_tty}'\n",
			wantBindKeyCalled: false,
		},
		{
			name:              "different command in same key triggers re-bind",
			key:               "n",
			command:           "run-shell '/new/cycle.sh next #{session_name} #{client_tty}'",
			listKeysOutput:    "bind-key -T prefix n run-shell '/old/cycle.sh next #{session_name} #{client_tty}'\n",
			wantBindKeyCalled: true,
			wantBindKeyArgs:   "bind-key -T prefix n",
			wantNotInLog:      []string{"if-shell"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binDir := t.TempDir()
			bindLog := filepath.Join(t.TempDir(), "tmux-bind.log")
			listKeysFile := filepath.Join(t.TempDir(), "list-keys-output.txt")
			if err := os.WriteFile(listKeysFile, []byte(tt.listKeysOutput), 0o644); err != nil {
				t.Fatalf("WriteFile listKeys: %v", err)
			}

			// Stub tmux:
			//   list-keys: emit controlled output from $LIST_KEYS_FILE
			//   bind-key:  log full argv to $TMUX_BIND_LOG
			//   else:      no-op
			writeExecutable(t, filepath.Join(binDir, "tmux"), `#!/bin/sh
# Drop a leading "-L <socket>" pair if present (cycle.sh-style guard).
if [ "$1" = "-L" ]; then
    shift 2
fi
case "$1" in
  list-keys)
    cat "$LIST_KEYS_FILE" 2>/dev/null
    ;;
  bind-key)
    printf '%s\n' "$*" >> "$TMUX_BIND_LOG"
    ;;
esac
exit 0
`)

			env := map[string]string{
				"PATH":           binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
				"TMUX_BIND_LOG":  bindLog,
				"LIST_KEYS_FILE": listKeysFile,
				"GC_TMUX_SOCKET": "", // disable -L flag so stub sees clean argv
			}

			cmd := exec.Command(bindKey, tt.key, tt.command)
			cmd.Env = mergeTestEnv(env)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("bind-key.sh failed: %v\n%s", err, out)
			}

			logBytes, _ := os.ReadFile(bindLog)
			log := string(logBytes)

			if !tt.wantBindKeyCalled {
				if log != "" {
					t.Fatalf("expected no bind-key call, got log: %q", log)
				}
				return
			}

			if !strings.Contains(log, tt.wantBindKeyArgs) {
				t.Fatalf("expected bind-key log to contain %q, got: %q", tt.wantBindKeyArgs, log)
			}
			for _, forbidden := range tt.wantNotInLog {
				if strings.Contains(log, forbidden) {
					t.Fatalf("expected bind-key log NOT to contain %q (regression), got: %q",
						forbidden, log)
				}
			}
		})
	}
}

// TestBindKeyScriptNoRecursiveWrapping asserts the structural property
// that ruled the original bug: no matter how many times bind-key.sh is
// invoked with the same args, the resulting binding is a single direct
// command, not a stack of wrapped if-shell layers. This is the property
// that hq-5vw7 (recursive wrapping) and hq-w1qlv ("command too long")
// both manifest under the prior shape.
func TestBindKeyScriptNoRecursiveWrapping(t *testing.T) {
	bindKey := filepath.Join(exampleDir(), "packs", "gastown", "assets", "scripts", "bind-key.sh")
	if _, err := os.Stat(bindKey); err != nil {
		t.Fatalf("bind-key.sh not found: %v", err)
	}

	binDir := t.TempDir()
	bindLog := filepath.Join(t.TempDir(), "tmux-bind.log")
	listKeysFile := filepath.Join(t.TempDir(), "list-keys-output.txt")

	// Stub tmux: each bind-key call updates the list-keys output so the
	// next bind-key.sh invocation "sees" the prior binding (simulating
	// what would happen across pack reinstalls / session_live re-fires).
	writeExecutable(t, filepath.Join(binDir, "tmux"), `#!/bin/sh
if [ "$1" = "-L" ]; then
    shift 2
fi
case "$1" in
  list-keys)
    cat "$LIST_KEYS_FILE" 2>/dev/null
    ;;
  bind-key)
    printf '%s\n' "$*" >> "$TMUX_BIND_LOG"
    # Update list-keys output to reflect the new binding.
    shift # drop "bind-key"
    if [ "$1" = "-T" ]; then
      table="$2"; key="$3"; shift 3
      printf 'bind-key -T %s %s %s\n' "$table" "$key" "$*" > "$LIST_KEYS_FILE"
    fi
    ;;
esac
exit 0
`)

	env := map[string]string{
		"PATH":           binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"TMUX_BIND_LOG":  bindLog,
		"LIST_KEYS_FILE": listKeysFile,
		"GC_TMUX_SOCKET": "",
	}

	command := "run-shell '/path/to/cycle.sh next #{session_name} #{client_tty}'"

	// Invoke bind-key.sh five times with the same args.
	for i := 0; i < 5; i++ {
		cmd := exec.Command(bindKey, "n", command)
		cmd.Env = mergeTestEnv(env)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("bind-key.sh iteration %d failed: %v\n%s", i, err, out)
		}
	}

	logBytes, _ := os.ReadFile(bindLog)
	log := string(logBytes)

	// First call binds, subsequent four are no-ops (idempotent).
	bindCount := strings.Count(log, "bind-key -T prefix n")
	if bindCount != 1 {
		t.Fatalf("expected exactly 1 bind-key invocation across 5 calls (idempotency); got %d:\n%s",
			bindCount, log)
	}
	if strings.Contains(log, "if-shell") {
		t.Fatalf("regression: bind-key log contains 'if-shell'; per-city socket isolation makes the wrap unnecessary:\n%s",
			log)
	}

	// Final list-keys file should show the direct binding, not a wrapped one.
	finalKeys, _ := os.ReadFile(listKeysFile)
	finalStr := string(finalKeys)
	if strings.Contains(finalStr, "if-shell") {
		t.Fatalf("regression: final binding contains 'if-shell':\n%s", finalStr)
	}
	if !strings.Contains(finalStr, command) {
		t.Fatalf("final binding does not contain expected command:\nbinding: %q\nwant substring: %q",
			finalStr, command)
	}
}
