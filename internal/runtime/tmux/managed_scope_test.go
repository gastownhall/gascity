package tmux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

func TestManagedScopeWrapsFinalModelArgumentsAndLiteralPrompt(t *testing.T) {
	for _, shape := range []string{"inline", "long-file", "no-prompt"} {
		t.Run(shape, func(t *testing.T) {
			dir := t.TempDir()
			launcher := filepath.Join(dir, "systemd-run")
			model := filepath.Join(dir, "codex")
			entry := filepath.Join(dir, "gc-entry")
			if err := os.WriteFile(entry, []byte("#!/bin/sh\n[ \"$1\" = exact-attempt ] || exit 97\nshift\nexec \"$@\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			// This is a tiny argv-preserving launcher spy, not a systemd/model
			// process. It rejects omission of the dollar-expansion protection.
			launchScript := "#!/bin/sh\nexpand=no\nfor arg do [ \"$arg\" != --expand-environment=no ] || expand=yes; done\n[ \"$expand\" = yes ] || exit 98\nwhile [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n"
			if err := os.WriteFile(launcher, []byte(launchScript), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(model, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\nprintf 'color:%s:%s\\n' \"${CI-unset}\" \"${NO_COLOR-unset}\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			prompt := "literal '$DOLLAR' ${NO_EXPAND} quote \" and space"
			if shape == "long-file" {
				prompt = strings.Repeat(prompt+";", 40)
			}
			cfg := runtime.Config{WorkDir: dir, Command: shellquote.Quote(model) + " --base " + shellquote.Quote("$BASE_LITERAL"), PromptFlag: "--prompt", PromptSuffix: shellquote.Quote(prompt), CommandScope: &runtime.CommandScope{Launcher: launcher, Unit: "gc-managed-owned-argument-test.scope", EntryCommand: []string{entry, "exact-attempt"}}}
			want := "--base\n$BASE_LITERAL\n--prompt\n" + prompt + "\ncolor:unset:unset\n"
			if shape == "no-prompt" {
				cfg.PromptSuffix = ""
				want = "--base\n$BASE_LITERAL\ncolor:unset:unset\n"
			}
			ops := &fakeStartOps{}
			if err := ensureFreshSession(ops, "owned-argument-test", cfg); err != nil {
				t.Fatal(err)
			}
			call := callsByMethod(t, ops, "createSession", 1)[0]
			args := shellquote.Split(call.command)
			if len(args) < 4 || args[len(args)-3] != "sh" || args[len(args)-2] != "-c" {
				t.Fatalf("not actual model command enclosure: %q", args)
			}
			// Reuse the provider's bounded real shell executor rather than
			// adding another subprocess runner just for the argv spy.
			outPath := filepath.Join(dir, "model-argv.txt")
			runner := &tmuxStartOps{tm: &Tmux{}}
			if err := runner.runSetupCommand(context.Background(), call.command+" > "+shellquote.Quote(outPath), map[string]string{
				"CI": "1", "NO_COLOR": "1", "DOLLAR": "must-not-expand", "NO_EXPAND": "must-not-expand", "BASE_LITERAL": "must-not-expand",
			}, 5*time.Second); err != nil {
				t.Fatal(err)
			}
			out, err := os.ReadFile(outPath)
			if err != nil || string(out) != want {
				t.Fatalf("model argv changed: err=%v got=%q want=%q", err, out, want)
			}
		})
	}
}

func TestManagedScopeDescriptorAlsoWrapsRespawnAndInvalidScopeRefuses(t *testing.T) {
	ops := &fakeStartOps{hasSessionResult: true}
	cfg := runtime.Config{Command: "codex --base", PromptFlag: "--prompt", PromptSuffix: shellquote.Quote("${LITERAL}"), CommandScope: &runtime.CommandScope{Launcher: "/usr/bin/systemd-run", Unit: "gc-managed-owned-respawn.scope", EntryCommand: []string{"/immutable/gc", "session", "capacity-enter", "/city", "sid", "attempt", "unit", "--"}}}
	if err := doRelaunchSession(context.Background(), ops, "owned-respawn", cfg, DefaultConfig().SetupTimeout); err != nil {
		t.Fatal(err)
	}
	call := callsByMethod(t, ops, "respawnAgent", 1)[0]
	args := shellquote.Split(call.command)
	if args[len(args)-1] != "env -u CI -u NO_COLOR codex --base --prompt '${LITERAL}'" {
		t.Fatalf("respawn lost final arguments: %q", args[len(args)-1])
	}
	cfg.CommandScope.Unit = "../../other.scope"
	if _, _, err := buildLaunchCommand("owned-invalid", cfg); err == nil {
		t.Fatal("invalid scope descriptor accepted")
	}
}
