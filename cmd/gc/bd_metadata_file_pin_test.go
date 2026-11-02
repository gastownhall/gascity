//go:build !windows

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/config"
)

// TestGcBdMetadataPipeReachesBd runs `gc bd create --metadata @<pipe>`
// through doBd on a single-store and a split city, with a stub bd that reads
// the --metadata file it is handed. The pipe fd is inherited by bd, as a
// shell's <(...) is. Every check and bd read the one copy gc made of the pipe,
// so bd reads the exact JSON gc checked.
func TestGcBdMetadataPipeReachesBd(t *testing.T) {
	const body = `{"k":"v"}`
	for _, c := range []struct{ name, storage string }{{"single-store", ""}, {"split", bdSQLRefusalSplitStorage}} {
		t.Run(c.name, func(t *testing.T) {
			capture, _ := bdSQLRefusalCityDir(t, c.storage)
			binDir := t.TempDir()
			stub := "#!/bin/sh\ncase \"$1\" in version) echo 'bd version 1.3.2 (test)'; exit 0 ;; esac\n" +
				"printf 'ARGV %s\\n' \"$*\" >> \"${CAPTURE_PATH}\"\n" +
				"m=''; prev=''\nfor a in \"$@\"; do case \"$a\" in --metadata=@*) m=\"${a#--metadata=@}\" ;; esac; if [ \"$prev\" = --metadata ]; then case \"$a\" in @*) m=\"${a#@}\" ;; esac; fi; prev=\"$a\"; done\n" +
				"if [ -n \"$m\" ]; then printf 'META %s [' \"$m\" >> \"${CAPTURE_PATH}\"; cat \"$m\" >> \"${CAPTURE_PATH}\" 2>&1; printf ']\\n' >> \"${CAPTURE_PATH}\"; fi\nexit 0\n"
			if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close() //nolint:errcheck // test pipe
			fd := r.Fd()
			// Let bd inherit the read end, as a shell's process substitution does.
			if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_SETFD, 0); errno != 0 {
				t.Fatal(errno)
			}
			if _, err := w.WriteString(body); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			code := doBd([]string{"create", "--title", "t", "--metadata", fmt.Sprintf("@/dev/fd/%d", fd)}, &stdout, &stderr)
			data, _ := os.ReadFile(capture)
			if code != 0 {
				t.Fatalf("exit %d; stderr=%q; bd saw %q", code, stderr.String(), data)
			}
			if !strings.Contains(string(data), "["+body+"]") {
				t.Fatalf("bd did not read the metadata gc checked: %q (stderr %q)", data, stderr.String())
			}
		})
	}
}

// TestGcBdMetadataPipeSurvivesTheRigCheck follows doBd's order: the pin, the
// rig-qualified metadata check (every city), the class judge (split), then bd.
// A pipe operand reaches the class judge and bd intact.
func TestGcBdMetadataPipeSurvivesTheRigCheck(t *testing.T) {
	const body = `{"k":"v"}`
	for _, c := range []struct {
		name string
		cfg  *config.City
	}{{"single-store", &config.City{}}, {"split", splitGuardTestCity()}} {
		t.Run(c.name, func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close() //nolint:errcheck // test pipe
			if _, err := w.WriteString(body); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			pipe := fmt.Sprintf("/dev/fd/%d", r.Fd())
			scope := t.TempDir()
			argv, unpin, err := bdPinMetadataFiles(scope, []string{"create", "--title", "t", "--metadata", "@" + pipe})
			if err != nil {
				t.Fatal(err)
			}
			defer unpin()
			if msg, refused := bdRigQualifiedMetadataRefusal(c.cfg, scope, argv); refused {
				t.Fatalf("rig check refused: %q", msg)
			}
			g := bdClassFlipGuard(c.cfg, scope, argv, judgedFilesDeps("bd-1.3.2"))
			if g.refusal != "" {
				t.Fatalf("class judge refused after the rig check read the pipe: %q", g.refusal)
			}
			if g.cleanup != nil {
				defer g.cleanup()
			}
			read := bdReadsFile(t, bdflags.InventoryByName("bd-1.3.2"), scope, g.args, "--metadata", "@")
			if got, err := os.ReadFile(read); err != nil || string(got) != body {
				t.Fatalf("forwarded %q: bd reads %s = %q (err %v), want %q", g.args, read, got, err, body)
			}
		})
	}
}

// TestBdPinMetadataFilesIsExact: the pin rewrites exactly the tokens pflag
// reads as a --metadata @file value of create, new or update, each read once
// into an absolute private copy that cleanup removes, and leaves every other
// argv alone.
func TestBdPinMetadataFilesIsExact(t *testing.T) {
	const body = `{"k":"v"}`
	cases := []struct {
		name   string
		argv   []string
		pinned []int
	}{
		{"separate", []string{"create", "--title", "t", "--metadata", "@m.json"}, []int{4}},
		{"inline", []string{"create", "--metadata=@m.json", "--title", "t"}, []int{1}},
		{"new alias", []string{"new", "--metadata", "@m.json"}, []int{2}},
		{"update", []string{"update", "demo-1", "--metadata", "@m.json"}, []int{3}},
		{"global flag before the verb", []string{"--actor", "bob", "update", "demo-1", "--metadata", "@m.json"}, []int{5}},
		{"twice", []string{"create", "--metadata", "@m.json", "--metadata", "@n.json"}, []int{2, 4}},
		{"a title spelled --metadata", []string{"create", "--title", "--metadata", "@m.json"}, nil},
		{"a short-flag run taking the next token", []string{"create", "-qd", "--metadata", "@m.json"}, nil},
		{"space-padded (bd reads it as JSON)", []string{"create", "--metadata", " @m.json"}, nil},
		{"after the terminator", []string{"create", "--", "--metadata", "@m.json"}, nil},
		{"a flag no pinned build has", []string{"create", "--no-such-flag", "--metadata", "@m.json"}, nil},
		{"unreadable", []string{"create", "--metadata", "@missing.json"}, nil},
		{"another verb", []string{"list", "--metadata", "@m.json"}, nil},
		{"inline JSON", []string{"create", "--metadata", body}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			scope := t.TempDir()
			for _, f := range []string{"m.json", "n.json"} {
				if err := os.WriteFile(filepath.Join(scope, f), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, cleanup, err := bdPinMetadataFiles(scope, c.argv)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(c.argv) {
				t.Fatalf("argv %q became %q", c.argv, got)
			}
			var copies []string
			for i := range c.argv {
				if slices.Contains(c.pinned, i) {
					v := got[i]
					if flag, rest, ok := strings.Cut(v, "="); ok && flag == "--metadata" {
						v = rest
					}
					p, ok := strings.CutPrefix(v, "@")
					if !ok || !filepath.IsAbs(p) || !strings.HasPrefix(filepath.Base(p), "gc-bd-metadata-") {
						t.Fatalf("token %d = %q, want @ and an absolute private copy", i, got[i])
					}
					copies = append(copies, p)
					continue
				}
				if got[i] != c.argv[i] {
					t.Fatalf("token %d rewritten: %q -> %q (argv %q)", i, c.argv[i], got[i], got)
				}
			}
			for _, f := range []string{"m.json", "n.json"} {
				if err := os.WriteFile(filepath.Join(scope, f), []byte(`{"gc.root_bead_id":"gcg-1"}`), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for _, p := range copies {
				if data, err := os.ReadFile(p); err != nil || string(data) != body {
					t.Fatalf("copy %s = %q (err %v), want the bytes read before the rewrite", p, data, err)
				}
			}
			cleanup()
			for _, p := range copies {
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Errorf("copy %s outlived cleanup (stat err %v)", p, err)
				}
			}
		})
	}
}

// TestBdWriteJudgedCopyPathIsAbsolute: bd runs in the scope root, so a copy
// written under a relative $TMPDIR must still be named by an absolute path.
func TestBdWriteJudgedCopyPathIsAbsolute(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.Mkdir("rel", 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", "rel")
	p, err := bdWriteJudgedCopy([]byte("x"), "gc-bd-judged-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(p) //nolint:errcheck // test copy
	if !filepath.IsAbs(p) {
		t.Fatalf("copy path %q is relative", p)
	}
	if data, err := os.ReadFile(p); err != nil || string(data) != "x" {
		t.Fatalf("copy %s = %q (err %v)", p, data, err)
	}
}
