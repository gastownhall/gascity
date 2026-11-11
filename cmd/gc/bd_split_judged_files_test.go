//go:build !windows

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beads"
)

// judgedFilesRead answers demo-abc, demo-1 and demo-2 as work beads of the
// work store; every other id is absent.
func judgedFilesRead(id string) (beads.Bead, bdReadResult) {
	switch id {
	case "demo-abc", "demo-1", "demo-2":
		return beads.Bead{ID: id, Type: "task"}, bdReadFound
	}
	return beads.Bead{}, bdReadNotFound
}

// judgedFilesDeps are splitTestDeps on the named pinned inventory.
func judgedFilesDeps(inventory string) bdClassGuardDeps {
	deps := splitTestDeps(nil, judgedFilesRead)
	deps.inventory = func() (*bdflags.Inventory, error) { return bdflags.InventoryByName(inventory), nil }
	return deps
}

// bdReadsFile returns the file bd reads for an admitted argv, resolved under
// inv as bd resolves it: flag's last value without prefix, or import's
// source.
func bdReadsFile(t *testing.T, inv *bdflags.Inventory, scope string, argv []string, flag, prefix string) string {
	t.Helper()
	res, err := inv.Resolve(argv)
	if err != nil {
		t.Fatalf("forwarded argv %q does not resolve: %v", argv, err)
	}
	var name string
	switch flag {
	case "":
		if v := res.Flags["--input"]; len(v) > 0 {
			name = v[len(v)-1]
		} else if len(res.Positionals) > 0 {
			name = res.Positionals[0]
		}
	default:
		v := res.Flags[flag]
		if len(v) == 0 {
			t.Fatalf("forwarded argv %q sets no %s", argv, flag)
		}
		var ok bool
		if name, ok = strings.CutPrefix(v[len(v)-1], prefix); !ok {
			t.Fatalf("forwarded %s=%q lacks the %q prefix", flag, v[len(v)-1], prefix)
		}
	}
	if !filepath.IsAbs(name) {
		name = filepath.Join(scope, name)
	}
	return name
}

// TestGcBdSplitHandsBdTheBytesItJudged: a split judge that reads a file the
// caller names hands bd a private copy of exactly the bytes it judged, in
// every spelling of the operand, under both pinned bd builds. The copy keeps
// the caller's base name in a private directory of its own. Rewriting the
// caller's file afterwards changes nothing bd reads, and the copy and its
// directory are gone once gc bd returns.
func TestGcBdSplitHandsBdTheBytesItJudged(t *testing.T) {
	const (
		script  = "create task 2 hello there\n"
		swapped = "create message 2 hello there\n"
		row     = `{"id":"demo-1","title":"w","issue_type":"task"}` + "\n"
		infra   = `{"id":"demo-1","title":"s","issue_type":"session","labels":["gc:session"]}` + "\n"
		meta    = `{"note":"x"}`
		graph   = `{"gc.root_bead_id":"gcg-1"}`
		plan    = `{"nodes":[{"key":"a","title":"a","type":"task"}]}`
		session = `{"nodes":[{"key":"s","title":"s","labels":["gc:session"]}]}`
	)
	cases := []struct {
		name, file, judged, swapped, flag, prefix string
		argv                                      []string
	}{
		{"batch --file", "ops.txt", script, swapped, "--file", "", []string{"batch", "--file", "ops.txt"}},
		{"batch --file=", "ops.txt", script, swapped, "--file", "", []string{"batch", "--file=ops.txt"}},
		{"batch -f", "ops.txt", script, swapped, "--file", "", []string{"batch", "-f", "ops.txt"}},
		{"batch -fX", "ops.txt", script, swapped, "--file", "", []string{"batch", "-fops.txt"}},
		{"batch --message spelling --file", "ops.txt", script, swapped, "--file", "", []string{"batch", "--message", "--file", "-f", "ops.txt"}},
		{"delete --from-file", "ids.txt", "demo-1\n", "demo-2\n", "--from-file", "", []string{"delete", "--from-file", "ids.txt"}},
		{"delete --from-file=", "ids.txt", "demo-1\n", "demo-2\n", "--from-file", "", []string{"delete", "--from-file=ids.txt"}},
		{"create --metadata @file", "m.json", meta, graph, "--metadata", "@", []string{"create", "--title", "t", "--metadata", "@m.json"}},
		{"create --metadata=@file", "m.json", meta, graph, "--metadata", "@", []string{"create", "--metadata=@m.json", "--title", "t"}},
		{"create --metadata @file before --", "m.json", meta, graph, "--metadata", "@", []string{"create", "--metadata", "@m.json", "--", "a title"}},
		{"create --title --", "m.json", meta, graph, "--metadata", "@", []string{"create", "--title", "--", "--metadata", "@m.json"}},
		{"update --metadata @file", "m.json", meta, graph, "--metadata", "@", []string{"update", "demo-abc", "--metadata", "@m.json"}},
		{"create --graph", "plan.json", plan, session, "--graph", "", []string{"create", "--graph", "plan.json", "--json"}},
		{"create --graph=", "plan.json", plan, session, "--graph", "", []string{"create", "--graph=plan.json"}},
		{"new --graph --dry-run", "plan.json", plan, session, "--graph", "", []string{"new", "--graph", "plan.json", "--dry-run"}},
		{"create --graph twice", "plan.json", plan, session, "--graph", "", []string{"create", "--graph", "other.json", "--graph", "plan.json"}},
		{"import <file>", "a.jsonl", row, infra, "", "", []string{"import", "a.jsonl"}},
		{"import -i", "a.jsonl", row, infra, "", "", []string{"import", "-i", "a.jsonl"}},
		{"import --input=", "a.jsonl", row, infra, "", "", []string{"import", "--input=a.jsonl"}},
		{"import --dry-run", "a.jsonl", row, infra, "", "", []string{"import", "a.jsonl", "--dry-run"}},
		{"import --input twice", "a.jsonl", row, infra, "", "", []string{"import", "--input", "other.jsonl", "-i", "a.jsonl"}},
		// A short-flag run ending in -i with its value attached, after an
		// earlier --input: pflag keeps the run's value, so bd reads it.
		{"import -qiFILE after --input", "a.jsonl", row, infra, "", "", []string{"import", "--input", "other.jsonl", "-qia.jsonl"}},
		{"import -vi=FILE after --input", "a.jsonl", row, infra, "", "", []string{"import", "--input", "other.jsonl", "-vi=a.jsonl"}},
		// After the terminator a flag-shaped name is the positional source.
		{"import -- -input.jsonl", "-input.jsonl", row, infra, "", "", []string{"import", "--", "-input.jsonl"}},
		{"import -- --archive.jsonl", "--archive.jsonl", row, infra, "", "", []string{"import", "--", "--archive.jsonl"}},
	}
	for _, inventory := range judgedFileInventories {
		inv := bdflags.InventoryByName(inventory)
		for _, c := range cases {
			t.Run(inventory+"/"+c.name, func(t *testing.T) {
				scope := t.TempDir()
				caller := filepath.Join(scope, c.file)
				if err := os.WriteFile(caller, []byte(c.judged), 0o644); err != nil {
					t.Fatal(err)
				}
				g := bdClassFlipGuard(splitGuardTestCity(), scope, c.argv, judgedFilesDeps(inventory))
				if g.refusal != "" {
					t.Fatalf("refused: %q", g.refusal)
				}
				if g.cleanup == nil {
					t.Fatalf("forwarded %q with no copy to remove", g.args)
				}
				read := bdReadsFile(t, inv, scope, g.args, c.flag, c.prefix)
				if read == caller {
					t.Fatalf("forwarded %q: bd re-reads the caller's %s", g.args, c.file)
				}
				if filepath.Base(read) != c.file {
					t.Errorf("forwarded %q: the copy %s does not keep the caller's name %s", g.args, read, c.file)
				}
				if fi, err := os.Stat(filepath.Dir(read)); err != nil || fi.Mode().Perm() != 0o700 {
					t.Errorf("the copy's directory %s is not private (%v, err %v)", filepath.Dir(read), fi.Mode(), err)
				}
				if fi, err := os.Stat(read); err != nil || fi.Mode().Perm() != 0o600 {
					t.Errorf("the copy %s is not private (%v, err %v)", read, fi.Mode(), err)
				}
				if err := os.WriteFile(caller, []byte(c.swapped), 0o644); err != nil {
					t.Fatal(err)
				}
				if got, err := os.ReadFile(read); err != nil || string(got) != c.judged {
					t.Fatalf("bd reads %q (err %v), gc judged %q", got, err, c.judged)
				}
				g.cleanup()
				if _, err := os.Stat(filepath.Dir(read)); !os.IsNotExist(err) {
					t.Errorf("the copy's directory %s outlived gc bd (stat err %v)", filepath.Dir(read), err)
				}
			})
		}
	}
}

// judgedFileInventories are the pinned bd builds the judged-file rows
// run under.
var judgedFileInventories = []string{"bd-1.3.2", bdEnterpriseInventory}

// TestGcBdSplitPipeOperandReachesBd: a pipe operand (process substitution,
// /dev/stdin) is drained when gc judges it; bd is handed the drained bytes,
// not the empty pipe, so `bd batch --file <(...)` runs the judged script
// instead of reporting "0 operations (no-op)".
func TestGcBdSplitPipeOperandReachesBd(t *testing.T) {
	for _, c := range []struct {
		name, body, flag string
		argv             func(string) []string
	}{
		{"batch --file", "create task 2 hello there\n", "--file", func(p string) []string { return []string{"batch", "--file", p} }},
		{"delete --from-file", "demo-1\n", "--from-file", func(p string) []string { return []string{"delete", "--from-file", p} }},
		{"import", `{"id":"demo-1","title":"w","issue_type":"task"}` + "\n", "", func(p string) []string { return []string{"import", p} }},
		{"create --graph", `{"nodes":[{"key":"a","title":"a","type":"task"}]}`, "--graph", func(p string) []string { return []string{"create", "--graph", p} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close() //nolint:errcheck // test pipe
			if _, err := w.WriteString(c.body); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			pipe := fmt.Sprintf("/dev/fd/%d", r.Fd())
			scope := t.TempDir()
			g := bdClassFlipGuard(splitGuardTestCity(), scope, c.argv(pipe), judgedFilesDeps("bd-1.3.2"))
			if g.refusal != "" {
				t.Fatalf("refused: %q", g.refusal)
			}
			if g.cleanup == nil {
				t.Fatalf("forwarded %q with no copy to remove", g.args)
			}
			defer g.cleanup()
			read := bdReadsFile(t, bdflags.InventoryByName("bd-1.3.2"), scope, g.args, c.flag, "")
			if got, err := os.ReadFile(read); err != nil || string(got) != c.body {
				t.Errorf("forwarded %q: bd reads %q (err %v), gc judged %q", g.args, got, err, c.body)
			}
		})
	}
}

// TestGcBdSplitRefusedFileWriteLeavesNoCopy: a refused write hands bd nothing,
// so no copy is written.
func TestGcBdSplitRefusedFileWriteLeavesNoCopy(t *testing.T) {
	scope := t.TempDir()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	for name, body := range map[string]string{
		"ops.txt": "create message 2 hello there\n",
		"a.jsonl": `{"id":"demo-1","title":"s","issue_type":"session","labels":["gc:session"]}` + "\n",
		"m.json":  `{"gc.root_bead_id":"gcg-1"}`,
		"p.json":  `{"nodes":[{"key":"s","title":"s","labels":["gc:session"]}]}`,
	} {
		if err := os.WriteFile(filepath.Join(scope, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, argv := range [][]string{
		{"batch", "--file", "ops.txt"},
		{"import", "a.jsonl"},
		{"create", "--title", "t", "--metadata", "@m.json"},
		{"create", "--graph", "p.json"},
	} {
		if g := bdClassFlipGuard(splitGuardTestCity(), scope, argv, judgedFilesDeps("bd-1.3.2")); g.refusal == "" {
			t.Errorf("gc bd %q was admitted", argv)
		}
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("a refused write left copies behind: %v", left)
	}
}

// judgedFileOperands writes one admissible caller file per kind of judged
// operand into scope and returns the argv that names each.
func judgedFileOperands(t *testing.T, scope string) [][]string {
	t.Helper()
	for name, body := range map[string]string{
		"ops.txt":   "create task 2 hello there\n",
		"ids.txt":   "demo-1\n",
		"m.json":    `{"note":"x"}`,
		"plan.json": `{"nodes":[{"key":"a","title":"a","type":"task"}]}`,
		"a.jsonl":   `{"id":"demo-1","title":"w","issue_type":"task"}` + "\n",
	} {
		if err := os.WriteFile(filepath.Join(scope, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return [][]string{
		{"batch", "--file", "ops.txt"},
		{"delete", "--from-file", "ids.txt"},
		{"create", "--title", "t", "--metadata", "@m.json"},
		{"create", "--graph", "plan.json"},
		{"import", "a.jsonl"},
	}
}

// TestGcBdSplitUnwritableTempDirIsRefused: a write gc admits but cannot hand
// bd a copy of (TMPDIR names a directory that does not exist) is refused,
// naming TMPDIR, before bd runs, and nothing is left behind.
func TestGcBdSplitUnwritableTempDirIsRefused(t *testing.T) {
	scope := t.TempDir()
	parent := t.TempDir()
	missing := filepath.Join(parent, "missing")
	t.Setenv("TMPDIR", missing)
	for _, argv := range judgedFileOperands(t, scope) {
		g := bdClassFlipGuard(splitGuardTestCity(), scope, argv, judgedFilesDeps("bd-1.3.2"))
		if !strings.Contains(g.refusal, missing+" (TMPDIR)") || !strings.HasSuffix(g.refusal, "Nothing was written.") {
			t.Errorf("gc bd %q: refusal %q, want one naming TMPDIR %s and that nothing was written", argv, g.refusal, missing)
		}
		if g.args != nil || g.cleanup != nil {
			t.Errorf("gc bd %q forwarded %q (cleanup set %v) despite the refusal", argv, g.args, g.cleanup != nil)
		}
	}
	if left, _ := os.ReadDir(parent); len(left) != 0 {
		t.Errorf("a refused write left something behind: %v", left)
	}
}

// TestBdJudgedFilesPinRemovesEveryCopyWhenOneFails: when one of a write's
// judged files cannot be handed over, the copies already written are removed
// with the refusal.
func TestBdJudgedFilesPinRemovesEveryCopyWhenOneFails(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	j := &bdJudgedFiles{files: []bdJudgedFile{
		{flag: "--metadata", prefix: "@", name: "m.json", data: []byte(`{"note":"x"}`)},
		// create has no --from-file, so no argv placement re-resolves.
		{flag: "--from-file", name: "ids.txt", data: []byte("demo-1\n")},
	}}
	g := j.pin(bdflags.InventoryByName("bd-1.3.2"), "create", bdClassGuard{args: []string{"create", "--title", "t", "--metadata", "@m.json"}})
	if !strings.Contains(g.refusal, "--from-file") || !strings.HasSuffix(g.refusal, "Nothing was written.") || g.args != nil {
		t.Fatalf("pin = %q (args %q), want the --from-file refusal", g.refusal, g.args)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("the refused write left copies behind: %v", left)
	}
}

// TestGcBdSplitJudgedCopyPathIsAbsolute: bd runs in the scope root, so a
// relative TMPDIR still hands bd an absolute copy path it can open.
func TestGcBdSplitJudgedCopyPathIsAbsolute(t *testing.T) {
	scope, cwd := t.TempDir(), t.TempDir()
	judgedFileOperands(t, scope)
	if err := os.Mkdir(filepath.Join(cwd, "rel"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	t.Setenv("TMPDIR", "rel")
	inv := bdflags.InventoryByName("bd-1.3.2")
	for _, c := range []struct {
		flag, prefix, judged string
		argv                 []string
	}{
		{"--file", "", "create task 2 hello there\n", []string{"batch", "--file", "ops.txt"}},
		{"--metadata", "@", `{"note":"x"}`, []string{"create", "--title", "t", "--metadata", "@m.json"}},
		{"", "", `{"id":"demo-1","title":"w","issue_type":"task"}` + "\n", []string{"import", "a.jsonl"}},
	} {
		g := bdClassFlipGuard(splitGuardTestCity(), scope, c.argv, judgedFilesDeps("bd-1.3.2"))
		if g.refusal != "" {
			t.Fatalf("gc bd %q refused: %q", c.argv, g.refusal)
		}
		read := bdReadsFile(t, inv, scope, g.args, c.flag, c.prefix)
		if got, err := os.ReadFile(read); err != nil || string(got) != c.judged {
			t.Errorf("gc bd %q forwarded %q: bd, in the scope root, reads %q (err %v), want the judged bytes", c.argv, g.args, got, err)
		}
		if g.cleanup != nil {
			g.cleanup()
		}
	}
}

// TestGcBdSplitMetadataFileIsReadAsBdReadsIt: bd reads `--metadata @<path>`
// from the path after the "@" exactly as written, trailing spaces included,
// so gc judges, and hands bd a copy of, that file and no other.
func TestGcBdSplitMetadataFileIsReadAsBdReadsIt(t *testing.T) {
	scope := t.TempDir()
	if err := os.WriteFile(filepath.Join(scope, "m.json"), []byte(`{"gc.root_bead_id":"gcg-1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	argv := []string{"update", "demo-abc", "--metadata", "@m.json "}
	if g := bdClassFlipGuard(splitGuardTestCity(), scope, argv, judgedFilesDeps("bd-1.3.2")); !strings.Contains(g.refusal, "m.json ") || !strings.Contains(g.refusal, "cannot be read") {
		t.Fatalf("gc bd %q judged a file bd does not read: %q", argv, g.refusal)
	}
	if err := os.WriteFile(filepath.Join(scope, "m.json "), []byte(`{"note":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	g := bdClassFlipGuard(splitGuardTestCity(), scope, argv, judgedFilesDeps("bd-1.3.2"))
	if g.refusal != "" {
		t.Fatalf("gc bd %q refused: %q", argv, g.refusal)
	}
	defer g.cleanup()
	read := bdReadsFile(t, bdflags.InventoryByName("bd-1.3.2"), scope, g.args, "--metadata", "@")
	if got, err := os.ReadFile(read); err != nil || string(got) != `{"note":"x"}` || filepath.Base(read) != "m.json " {
		t.Errorf("bd is handed %s holding %q (err %v), want a copy of %q", read, got, err, "m.json ")
	}
}

// TestGcBdSplitSpacePaddedMetadataFileIsJudgedNotHandedOver: bd reads a
// --metadata file only for a value that starts with "@"; a space-padded one it
// rejects as JSON. gc still judges the file, but forwards the value unchanged.
func TestGcBdSplitSpacePaddedMetadataFileIsJudgedNotHandedOver(t *testing.T) {
	scope := t.TempDir()
	if err := os.WriteFile(filepath.Join(scope, "m.json"), []byte(`{"gc.root_bead_id":"gcg-1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	argv := []string{"create", "--title", "t", "--metadata", " @m.json"}
	if g := bdClassFlipGuard(splitGuardTestCity(), scope, argv, judgedFilesDeps("bd-1.3.2")); !strings.Contains(g.refusal, "graph-class") {
		t.Fatalf("a space-padded @file was not judged: %q", g.refusal)
	}
	if err := os.WriteFile(filepath.Join(scope, "m.json"), []byte(`{"note":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	g := bdClassFlipGuard(splitGuardTestCity(), scope, argv, judgedFilesDeps("bd-1.3.2"))
	if g.refusal != "" || !slices.Equal(g.args, argv) || g.cleanup != nil {
		t.Fatalf("forwarded %q (refusal %q), want argv unchanged with no copy", g.args, g.refusal)
	}
}

// TestBdRepointFlagIsExact: the repointed argv resolves to the same command,
// positionals and flags but for the one flag, or no argv is returned.
func TestBdRepointFlagIsExact(t *testing.T) {
	inv := bdflags.InventoryByName("bd-1.3.2")
	for _, c := range []struct {
		name string
		argv []string
		want []string
	}{
		{"appended", []string{"batch", "--file", "a"}, []string{"batch", "--file", "a", "--file=/c"}},
		{"before the terminator", []string{"create", "--metadata", "@a", "--", "t"}, []string{"create", "--metadata", "@a", "--file=/c", "--", "t"}},
		{"a -- value is not a terminator", []string{"create", "--title", "--", "--file", "a"}, []string{"create", "--title", "--", "--file", "a", "--file=/c"}},
	} {
		got, ok := bdRepointFlag(inv, c.argv, "--file", "/c")
		if !ok || !slices.Equal(got, c.want) {
			t.Errorf("%s: bdRepointFlag(%q) = %q, %v; want %q", c.name, c.argv, got, ok, c.want)
		}
	}
	if got, ok := bdRepointFlag(inv, []string{"batch", "--no-such-flag"}, "--file", "/c"); ok {
		t.Errorf("an argv bd rejects was repointed: %q", got)
	}
	if got, ok := bdRepointFlag(nil, []string{"batch"}, "--file", "/c"); ok {
		t.Errorf("repointed with no inventory: %q", got)
	}
}

// TestBdRepointPositionalIsExact: only the token bd reads as the positional
// is rewritten, and the result resolves to the same command and flags, or no
// argv is returned.
func TestBdRepointPositionalIsExact(t *testing.T) {
	inv := bdflags.InventoryByName("bd-1.3.2")
	for _, c := range []struct {
		name, from string
		argv, want []string
	}{
		{"plain", "a.jsonl", []string{"import", "a.jsonl"}, []string{"import", "/c"}},
		{"a flag value spelled like the source", "a.jsonl", []string{"import", "--actor", "a.jsonl", "a.jsonl"}, []string{"import", "--actor", "a.jsonl", "/c"}},
		{"-i-shaped name after --", "-input.jsonl", []string{"import", "--", "-input.jsonl"}, []string{"import", "--", "/c"}},
		{"--shaped name after --", "--archive.jsonl", []string{"import", "--", "--archive.jsonl"}, []string{"import", "--", "/c"}},
	} {
		got, ok := bdRepointPositional(inv, c.argv, c.from, "/c")
		if !ok || !slices.Equal(got, c.want) {
			t.Errorf("%s: bdRepointPositional(%q) = %q, %v; want %q", c.name, c.argv, got, ok, c.want)
		}
	}
	if got, ok := bdRepointPositional(inv, []string{"import", "--actor", "a.jsonl"}, "a.jsonl", "/c"); ok {
		t.Errorf("a flag value was repointed as the positional: %q", got)
	}
	if got, ok := bdRepointPositional(nil, []string{"import", "a.jsonl"}, "a.jsonl", "/c"); ok {
		t.Errorf("repointed with no inventory: %q", got)
	}
}

// TestGcBdSplitBatchFileEndToEnd runs gc bd batch --file through doBd on a
// split city: bd is handed a private copy holding the judged script, and the
// copy is removed when gc bd returns.
func TestGcBdSplitBatchFileEndToEnd(t *testing.T) {
	capture, _ := bdSQLRefusalCityDir(t, bdSQLRefusalSplitStorage)
	binDir := t.TempDir()
	stub := "#!/bin/sh\ncase \"$1\" in version) echo 'bd version 1.3.2 (test)'; exit 0 ;; esac\n" +
		"printf '%s\\n' \"$*\" >> \"${CAPTURE_PATH}\"\n" +
		"f=''\nfor a in \"$@\"; do case \"$a\" in --file=*) f=\"${a#--file=}\" ;; esac; done\n" +
		"if [ -n \"$f\" ]; then printf 'COPY %s\\n' \"$f\" >> \"${CAPTURE_PATH}\"; cat \"$f\" >> \"${CAPTURE_PATH}\"; fi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ops := filepath.Join(t.TempDir(), "ops.txt")
	if err := os.WriteFile(ops, []byte("create task 2 hello there\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"batch", "--file", ops}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, stderr.String())
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	_, after, ok := strings.Cut(got, "COPY ")
	if !ok || !strings.Contains(got, "create task 2 hello there") {
		t.Fatalf("bd was not handed the judged script: %q", got)
	}
	copyPath, _, _ := strings.Cut(after, "\n")
	if copyPath == ops {
		t.Fatalf("bd re-read the caller's file: %q", got)
	}
	if _, err := os.Stat(copyPath); !os.IsNotExist(err) {
		t.Errorf("the copy %s outlived gc bd (stat err %v)", copyPath, err)
	}
}
