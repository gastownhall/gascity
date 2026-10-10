package testenv

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// This file is stdlib-only on purpose: every test binary in the repo imports
// the package, so whatever it imports links into all of them.

// gitTemplateEntry is one entry of the git template: a directory, or a regular
// file holding content.
type gitTemplateEntry struct {
	rel     string // slash-separated, relative to the template
	dir     bool
	content string
}

// gitTemplateEntries is everything the git template holds, parents before
// children. config turns off git's detached auto-maintenance (ga-zoe1wr). The
// empty hooks/ and info/exclude are there because the default template would
// have provided them and a repo without them surprises whatever writes a hook
// or an ignore rule into it. Nothing else is in the template, and nothing else
// is accepted in it.
var gitTemplateEntries = []gitTemplateEntry{
	{rel: "config", content: "# Seeded by internal/testenv (ga-zoe1wr): test repos must not spawn detached\n" +
		"# git auto-maintenance, which races t.TempDir cleanup. See TESTING.md.\n" +
		"[maintenance]\n\tauto = false\n"},
	{rel: "hooks", dir: true},
	{rel: "info", dir: true},
	{rel: "info/exclude"},
}

// gitTemplateName names the template directory: the owner's uid keeps users
// who share a temp dir from meeting each other's template, and a digest of
// gitTemplateEntries means a template written by a different layout is a
// different directory, never mistaken for this one.
func gitTemplateName() string {
	var layout strings.Builder
	for _, e := range gitTemplateEntries {
		layout.WriteString(e.rel)
		layout.WriteByte(0)
		layout.WriteString(strconv.FormatBool(e.dir))
		layout.WriteByte(0)
		layout.WriteString(strconv.Itoa(len(e.content)))
		layout.WriteByte(0)
		layout.WriteString(e.content)
		layout.WriteByte(0)
	}
	sum := sha256.Sum256([]byte(layout.String()))
	return "gc-test-gittemplate-" + strconv.Itoa(os.Getuid()) + "-" + hex.EncodeToString(sum[:])[:12]
}

// gitTemplateRoot is the directory the template goes in: $TEST_TMPDIR when
// Bazel sets it, which owns and cleans that directory, else the temp dir.
func gitTemplateRoot() string {
	if dir := os.Getenv("TEST_TMPDIR"); dir != "" {
		return dir
	}
	return os.TempDir()
}

// seedGitTemplate points GIT_TEMPLATE_DIR at the template, replacing any
// ambient one, so every repo this test binary or its children create with git
// init or git clone starts with maintenance.auto=false. It panics when the
// template cannot be had, since a test run without it is the flaky run it
// exists to prevent.
func seedGitTemplate() {
	dir, err := ensureGitTemplate(gitTemplateRoot())
	if err != nil {
		panic("testenv: " + err.Error())
	}
	if err := os.Setenv("GIT_TEMPLATE_DIR", dir); err != nil {
		panic("testenv: pointing GIT_TEMPLATE_DIR at the git template " + dir + ": " + err.Error())
	}
}

// ensureGitTemplate returns the template directory under root, writing it
// first if it is not there. What it returns has been verified against
// gitTemplateEntries, whoever wrote it.
func ensureGitTemplate(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolving the git template directory %q: %w", root, err)
	}
	target := filepath.Join(abs, gitTemplateName())
	if _, err := os.Lstat(target); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("looking for the git template: %w", err)
		}
		if err := installGitTemplate(abs, target); err != nil {
			return "", err
		}
		return target, nil
	}
	if err := verifyGitTemplate(target); err != nil {
		return "", err
	}
	return target, nil
}

// installGitTemplate builds the template in a temp dir beside target and
// renames it into place, so no process ever sees a half-written template at
// target. A rename that fails because target now exists means another process
// installed it first; that one is verified like any existing template, and
// this one's temp dir is removed.
func installGitTemplate(root, target string) (err error) {
	tmp, err := os.MkdirTemp(root, filepath.Base(target)+".tmp-")
	if err != nil {
		return fmt.Errorf("cannot create the git template under %s (TEST_TMPDIR, else the temp dir, must be a writable directory): %w", root, err)
	}
	installed := false
	defer func() {
		if installed {
			return
		}
		if rmErr := os.RemoveAll(tmp); rmErr != nil {
			err = errors.Join(err, fmt.Errorf("remove the leftover %s yourself: %w", tmp, rmErr))
		}
	}()

	if err := writeGitTemplate(tmp); err != nil {
		return fmt.Errorf("writing the git template in %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		if _, statErr := os.Lstat(target); statErr != nil {
			return fmt.Errorf("moving the git template into place at %s: %w", target, err)
		}
		return verifyGitTemplate(target)
	}
	installed = true
	return verifyGitTemplate(target)
}

// writeGitTemplate writes gitTemplateEntries into dir, which exists and is empty.
func writeGitTemplate(dir string) error {
	for _, e := range gitTemplateEntries {
		p := filepath.Join(dir, filepath.FromSlash(e.rel))
		if e.dir {
			if err := os.Mkdir(p, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.WriteFile(p, []byte(e.content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// verifyGitTemplate checks that dir holds gitTemplateEntries and nothing
// else, each a real directory or regular file (never a symlink) with the
// expected content, and on unix owned by this user and not writable by anyone
// else. Git copies a template's hooks/ into every repo made from it, so a
// template that differs in any way is not used; the error names the first
// entry that differs and the directory to remove.
func verifyGitTemplate(dir string) error {
	if err := verifyGitTemplateEntries(dir); err != nil {
		return fmt.Errorf("%w; the template is not used, because git copies it, hooks included, into every test repo: remove %s and run the tests again", err, dir)
	}
	return nil
}

// verifyGitTemplateEntries is verifyGitTemplate without the advice on what to
// do about the template: the first way dir differs from gitTemplateEntries.
func verifyGitTemplateEntries(dir string) error {
	uid := os.Getuid()
	if err := verifyTemplateEntry(dir, gitTemplateEntry{rel: ".", dir: true}, uid); err != nil {
		return err
	}
	children := map[string][]string{} // directory (slash-separated, "." for dir) -> names it holds
	for _, e := range gitTemplateEntries {
		if err := verifyTemplateEntry(filepath.Join(dir, filepath.FromSlash(e.rel)), e, uid); err != nil {
			return err
		}
		children[path.Dir(e.rel)] = append(children[path.Dir(e.rel)], path.Base(e.rel))
	}
	for _, e := range append([]gitTemplateEntry{{rel: ".", dir: true}}, gitTemplateEntries...) {
		if !e.dir {
			continue
		}
		abs := filepath.Join(dir, filepath.FromSlash(e.rel))
		entries, err := os.ReadDir(abs)
		if err != nil {
			return fmt.Errorf("%s: %w", abs, err)
		}
		for _, de := range entries {
			if !slices.Contains(children[e.rel], de.Name()) {
				return fmt.Errorf("%s is not part of the template", filepath.Join(abs, de.Name()))
			}
		}
	}
	return nil
}

// verifyTemplateEntry checks the entry at p against want: its kind, who owns
// it and who can write it, and for a file its content.
func verifyTemplateEntry(p string, want gitTemplateEntry, uid int) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if want.dir && !fi.IsDir() || !want.dir && !fi.Mode().IsRegular() {
		kind := "a regular file"
		if want.dir {
			kind = "a directory"
		}
		return fmt.Errorf("%s is %s, not %s", p, describeMode(fi), kind)
	}
	if err := checkTemplateEntry(p, fi, uid); err != nil {
		return err
	}
	if want.dir {
		return nil
	}
	got, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if string(got) != want.content {
		return fmt.Errorf("%s does not hold what the template writes there", p)
	}
	return nil
}

// describeMode names the kind of file fi describes, for an error that says it
// is not the kind the template needs.
func describeMode(fi os.FileInfo) string {
	switch m := fi.Mode(); {
	case m&os.ModeSymlink != 0:
		return "a symlink"
	case m.IsDir():
		return "a directory"
	case m.IsRegular():
		return "a regular file"
	default:
		return "a special file"
	}
}
