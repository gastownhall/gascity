// Package genshelltest runs generated programs in tests against fake bd and gc
// written as #!/bin/sh scripts, the form the fakes in upstream's tests take.
package genshelltest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/genshell"
)

// ScriptFakes is a genshell.ExecFunc that, in place of running p, runs the
// #!/bin/sh script named p.Args[0] that a test put on p's PATH, in the same
// embedded interpreter, with p's arguments, directory, environment and
// streams. It never starts a process: a program with no such script on PATH
// fails as not found, and a file that is not a #!/bin/sh script fails too.
func ScriptFakes(ctx context.Context, p genshell.Process) error {
	path, err := scriptOnPath(p.Env, p.Args[0])
	if err != nil {
		return err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(string(body), "#!/bin/sh\n") {
		return fmt.Errorf("%s: not a #!/bin/sh script", path)
	}
	return genshell.Program{Script: string(body), Name: path, Args: p.Args[1:]}.Run(ctx, genshell.Options{
		Dir:    p.Dir,
		Env:    p.Env,
		Stdin:  p.Stdin,
		Stdout: p.Stdout,
		Stderr: p.Stderr,
		Exec:   ScriptFakes,
	})
}

// scriptOnPath returns the first regular file named exactly name in the PATH
// of env.
func scriptOnPath(env []string, name string) (string, error) {
	var pathList string
	for _, kv := range env {
		if key, value, ok := strings.Cut(kv, "="); ok && strings.EqualFold(key, "PATH") {
			pathList = value
		}
	}
	for _, dir := range filepath.SplitList(pathList) {
		if dir == "" {
			continue
		}
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path, nil
		}
	}
	return "", fmt.Errorf("%s: no fake script on PATH", name)
}
