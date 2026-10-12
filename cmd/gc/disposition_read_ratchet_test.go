package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// dispositionRawReadsBaseline is the per-file count TestDispositionReadRatchet
// may not exceed, and must be lowered to when a file's count falls.
const dispositionRawReadsBaseline = "testdata/disposition_raw_reads.golden"

// dispositionReadScanDirs are the trees whose raw reads the ratchet counts,
// each with every package under it.
var dispositionReadScanDirs = []string{"cmd/gc", "internal/session", "internal/api"}

// dispositionCodecFiles are the codec, and the field registry that names
// every key, exempt by exact name: a new file beside them is counted.
var dispositionCodecFiles = []string{"internal/session/disposition.go", "internal/session/disposition_patch.go", "internal/session/disposition_store.go", "internal/session/fields.go"}

// dispositionInfoFields and dispositionMetaKeys are the five disposition keys
// as session.Info fields and as metadata keys.
var (
	dispositionInfoFields = []string{"HeldUntil", "SleepIntent", "WaitHold", "QuarantinedUntil", "SuspendedAt"}
	dispositionMetaKeys   = []string{"held_until", "sleep_intent", "wait_hold", "quarantined_until", "suspended_at"}
)

// TestDispositionReadRatchet is R7's raw-read ratchet (ARCH-RESTRUCTURE-2
// §5.1): per production file, the reads of the five disposition keys outside
// the codec (dispositionCodecFiles), as an Info field (x.HeldUntil), a
// metadata index (m["held_until"]) that is not an assignment's target, a
// key's string passed to a call (a helper that reads it), or a constant or
// variable declared to a key's string (an alias a read could index by).
// A reader decodes the disposition (DecodeDisposition, DispositionOfInfo)
// and reads a projection instead; the count may only fall. It matches by
// name, so a same-named field of another type (a startup episode's
// QuarantinedUntil) counts too; the baseline carries it.
func TestDispositionReadRatchet(t *testing.T) {
	root := repoRootForLint(t)
	got := map[string]int{}
	files := 0
	for _, dir := range dispositionReadScanDirs {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			switch {
			case d.IsDir() && d.Name() == "testdata":
				return filepath.SkipDir
			case d.IsDir(), !strings.HasSuffix(rel, ".go"), strings.HasSuffix(rel, "_test.go"), slices.Contains(dispositionCodecFiles, rel):
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
			if err != nil {
				return fmt.Errorf("parse %s: %w", p, err)
			}
			files++
			if n := dispositionRawReads(f); n > 0 {
				got[rel] = n
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	want := readDispositionReadsBaseline(t)
	total, wantTotal := 0, 0
	for rel, n := range got {
		total += n
		if n > want[rel] {
			t.Errorf("%s: %d raw disposition reads, baseline %d: read a Disposition projection", rel, n, want[rel])
		}
	}
	for rel, n := range want {
		wantTotal += n
		if got[rel] < n {
			t.Errorf("%s: %d raw disposition reads, below its baseline %d: lower %s", rel, got[rel], n, dispositionRawReadsBaseline)
		}
	}
	if t.Failed() {
		var lines []string
		for rel, n := range got {
			lines = append(lines, fmt.Sprintf("%s %d", rel, n))
		}
		slices.Sort(lines)
		t.Logf("counted %d (baseline %d):\n%s", total, wantTotal, strings.Join(lines, "\n"))
	}
	if total == 0 || files < 100 {
		t.Fatalf("counted %d reads in %d files; the scan missed the tree", total, files)
	}
}

// dispositionKeyLit reports a string literal of one of the five keys.
func dispositionKeyLit(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	key, err := strconv.Unquote(lit.Value)
	return err == nil && slices.Contains(dispositionMetaKeys, key)
}

// dispositionRawReads counts f's raw reads of the five keys.
func dispositionRawReads(f *ast.File) int {
	targets := map[ast.Expr]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if as, ok := n.(*ast.AssignStmt); ok {
			for _, lhs := range as.Lhs {
				targets[lhs] = true
			}
		}
		return true
	})
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		switch x := node.(type) {
		case *ast.SelectorExpr:
			if slices.Contains(dispositionInfoFields, x.Sel.Name) && !targets[x] {
				n++
			}
		case *ast.CallExpr:
			for _, arg := range x.Args {
				if dispositionKeyLit(arg) {
					n++
				}
			}
		case *ast.ValueSpec:
			for _, v := range x.Values {
				if dispositionKeyLit(v) {
					n++
				}
			}
		case *ast.IndexExpr:
			if dispositionKeyLit(x.Index) && !targets[x] {
				n++
			}
		}
		return true
	})
	return n
}

func readDispositionReadsBaseline(t *testing.T) map[string]int {
	t.Helper()
	f, err := os.Open(dispositionRawReadsBaseline)
	if err != nil {
		t.Fatalf("reading the baseline: %v", err)
	}
	defer func() { _ = f.Close() }()
	out := map[string]int{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rel, count, ok := strings.Cut(line, " ")
		n, err := strconv.Atoi(count)
		if !ok || err != nil {
			t.Fatalf("baseline line %q", line)
		}
		out[rel] = n
	}
	return out
}
