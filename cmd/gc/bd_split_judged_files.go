package main

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/gastownhall/gascity/internal/bdflags"
)

// Some split-city judges read part of a write from a file the caller names:
// `batch --file`, `delete --from-file`, `create` and `update --metadata @file`,
// `create --graph <plan>`, and `import <file>`. bd opens that path again after
// gc has judged it, so without a copy bd writes what the path holds then, not
// what gc judged: a pipe the judge drained (`<(...)`, /dev/stdin) reads empty,
// and `bd batch` reports "0 operations (no-op)" and exits 0; a file rewritten
// between the two reads is written unjudged. A judge therefore reads such a
// file through readJudgedFile, and once it admits the write, bdClassFlipGuard
// points bd at a private copy of exactly the bytes judged (bdJudgedFiles.pin),
// written by bdWriteJudgedCopy. import writes its own copy, because it may
// hand bd only some of the rows, and repoints its source with the same
// verified rewrites (bdRepointFlag, bdRepointPositional).

// bdJudgedFile is one caller-named file a judge read.
type bdJudgedFile struct {
	// flag is the long string flag bd reads the file from ("--file").
	flag string
	// prefix is what bd expects before the path in the flag's value ("@"
	// for --metadata).
	prefix string
	// name is the path as the caller spelled it; the copy keeps its base
	// name.
	name string
	data []byte
}

// bdJudgedFiles collects the files one judgment read.
type bdJudgedFiles struct {
	files []bdJudgedFile
}

// readScopeFile reads the file name relative to the scope root: bd runs
// there, so a relative path names the same file for gc and bd.
func (in bdClassGuardInput) readScopeFile(name string) ([]byte, error) {
	path := name
	if !filepath.IsAbs(path) {
		path = filepath.Join(in.scopeRoot, path)
	}
	return os.ReadFile(path)
}

// readJudgedFile is readScopeFile for a file bd reads from the string flag
// flag (its value is prefix and the path): the bytes are recorded so that an
// admitted write hands bd exactly them.
func (in bdClassGuardInput) readJudgedFile(flag, prefix, name string) ([]byte, error) {
	data, err := in.readScopeFile(name)
	if err != nil {
		return nil, err
	}
	if in.judged != nil {
		in.judged.files = append(in.judged.files, bdJudgedFile{flag: flag, prefix: prefix, name: name, data: data})
	}
	return data, nil
}

// pin points each recorded flag of an admitted write at a private copy of
// the bytes judged; the copies are removed when gc bd returns. A copy gc
// cannot write, or an argv gc cannot repoint exactly, refuses the write.
func (j *bdJudgedFiles) pin(inv *bdflags.Inventory, cmdPath string, g bdClassGuard) bdClassGuard {
	for _, f := range j.files {
		copyPath, remove, err := bdWriteJudgedCopy(f.data, f.name)
		if err == nil {
			args, ok := bdRepointFlag(inv, g.args, f.flag, f.prefix+copyPath)
			if ok {
				g.args = args
				prev := g.cleanup
				g.cleanup = func() {
					remove()
					if prev != nil {
						prev()
					}
				}
				continue
			}
			remove()
			err = fmt.Errorf("its argv cannot be repointed at the copy exactly (spell it as %s <file>)", f.flag)
		}
		if g.cleanup != nil {
			g.cleanup()
		}
		return bdClassGuard{refusal: fmt.Sprintf("gc bd %s: refused on this split city: gc hands bd a private copy of the %s file it judged, and could not: %v. Nothing was written.", cmdPath, f.flag, err)}
	}
	return g
}

// bdWriteJudgedCopy writes data into a new private directory (0700) under the
// temp directory, as a file (0600) with the base name of source, the path the
// caller gave, and returns the copy's path and a func that removes the
// directory. The path is absolute: bd runs in the scope root and would resolve
// a relative TMPDIR there. The copy keeps the caller's base name because bd
// names the file it reads where it records the write (an import's history
// entry is "bd import: N issues from <name>").
func bdWriteJudgedCopy(data []byte, source string) (string, func(), error) {
	tmpDir, err := filepath.Abs(os.TempDir())
	if err != nil {
		return "", nil, fmt.Errorf("the temp directory %s (TMPDIR) cannot be resolved: %w", os.TempDir(), err)
	}
	dir, err := os.MkdirTemp(tmpDir, "gc-bd-judged-*")
	if err != nil {
		return "", nil, fmt.Errorf("writing it under the temp directory %s (TMPDIR) failed: %w", tmpDir, err)
	}
	remove := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, filepath.Base(source))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		remove()
		return "", nil, fmt.Errorf("writing it under the temp directory %s (TMPDIR) failed: %w", tmpDir, err)
	}
	return path, remove, nil
}

// bdRepointFlag returns argv with the string flag long set to value. pflag
// keeps a repeated string flag's last value, so `long=value` is appended, or
// put just before a `--` that ends the flags. The result must resolve, under
// inv as bd resolves it, to the same command, positionals and flags as argv
// but for long; ok is false when no placement does.
func bdRepointFlag(inv *bdflags.Inventory, argv []string, long, value string) ([]string, bool) {
	if inv == nil {
		return nil, false
	}
	want, err := inv.Resolve(argv)
	if err != nil {
		return nil, false
	}
	token := long + "=" + value
	candidates := [][]string{append(slices.Clone(argv), token)}
	for i, tok := range argv {
		if tok == "--" {
			candidates = append(candidates, slices.Insert(slices.Clone(argv), i, token))
		}
	}
	for _, c := range candidates {
		got, err := inv.Resolve(c)
		if err == nil && bdResolvedSameExcept(want, got, long, value) {
			return c, true
		}
	}
	return nil, false
}

// bdRepointPositional returns argv with the positional argument from replaced
// by to, rewriting the token in place. The result must resolve, under inv as bd
// resolves it, to the same command and flags as argv, and to the same
// positionals with from replaced by to; a token that equals from but is a flag's
// value (or one after a terminator that bd reads differently) never verifies.
// ok is false when no token does.
func bdRepointPositional(inv *bdflags.Inventory, argv []string, from, to string) ([]string, bool) {
	if inv == nil {
		return nil, false
	}
	want, err := inv.Resolve(argv)
	if err != nil {
		return nil, false
	}
	at := slices.Index(want.Positionals, from)
	if at < 0 {
		return nil, false
	}
	expect := slices.Clone(want.Positionals)
	expect[at] = to
	for i, tok := range argv {
		if tok != from {
			continue
		}
		c := slices.Clone(argv)
		c[i] = to
		got, err := inv.Resolve(c)
		if err == nil && got.Path == want.Path && got.Help == want.Help && got.DisableFlagParsing == want.DisableFlagParsing &&
			slices.Equal(got.Positionals, expect) && maps.EqualFunc(got.Flags, want.Flags, slices.Equal[[]string]) {
			return c, true
		}
	}
	return nil, false
}

// bdResolvedSameExcept reports whether got is want with the flag long set to
// value and nothing else changed.
func bdResolvedSameExcept(want, got bdflags.Resolved, long, value string) bool {
	if got.Path != want.Path || got.Help != want.Help || got.DisableFlagParsing != want.DisableFlagParsing ||
		!slices.Equal(got.Positionals, want.Positionals) {
		return false
	}
	if v := got.Flags[long]; len(v) != 1 || v[0] != value {
		return false
	}
	for name, v := range want.Flags {
		if name != long && !slices.Equal(got.Flags[name], v) {
			return false
		}
	}
	for name := range got.Flags {
		if _, ok := want.Flags[name]; !ok && name != long {
			return false
		}
	}
	return true
}
