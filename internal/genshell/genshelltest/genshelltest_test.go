package genshelltest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/genshell"
)

func TestScriptFakesRunsTheFirstScriptOnPath(t *testing.T) {
	early, late := t.TempDir(), t.TempDir()
	write := func(dir, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "bd"), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(early, "#!/bin/sh\nprintf 'early:%s:%s' \"$1\" \"$GC_PROBE\"\nexit 3\n")
	write(late, "#!/bin/sh\nprintf 'late'\n")

	var out bytes.Buffer
	err := ScriptFakes(context.Background(), genshell.Process{
		Args:   []string{"bd", "ready"},
		Dir:    t.TempDir(),
		Env:    []string{"PATH=" + early + string(os.PathListSeparator) + late, "GC_PROBE=env"},
		Stdout: &out,
	})
	if got, want := out.String(), "early:ready:env"; got != want {
		t.Errorf("output = %q, want %q: the first script on PATH, with the arguments and environment", got, want)
	}
	var exit *genshell.ExitError
	if !errors.As(err, &exit) || exit.Status != 3 {
		t.Errorf("err = %v, want the script's exit status 3", err)
	}
}

func TestScriptFakesRunsNothingElse(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gc"), []byte("MZ not a script"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + dir}
	for name, want := range map[string]string{"bd": "no fake script on PATH", "gc": "not a #!/bin/sh script"} {
		err := ScriptFakes(context.Background(), genshell.Process{Args: []string{name}, Env: env, Stdout: &bytes.Buffer{}})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", name, err, want)
		}
	}
}
