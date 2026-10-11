package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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

// dispositionReadScanDirs are the packages whose raw reads the ratchet counts.
var dispositionReadScanDirs = []string{"cmd/gc", "internal/session", "internal/api"}

// dispositionInfoFields and dispositionMetaKeys are the five disposition keys
// as session.Info fields and as metadata keys.
var (
	dispositionInfoFields = []string{"HeldUntil", "SleepIntent", "WaitHold", "QuarantinedUntil", "SuspendedAt"}
	dispositionMetaKeys   = []string{"held_until", "sleep_intent", "wait_hold", "quarantined_until", "suspended_at"}
)

// TestDispositionReadRatchet is R7's raw-read ratchet (ARCH-RESTRUCTURE-2
// §5.1): per production file, the reads of the five disposition keys outside
// the codec (internal/session/disposition*.go), as an Info field (x.HeldUntil)
// or a metadata index (m["held_until"]) that is not an assignment's target.
// A reader decodes the disposition (DecodeDisposition, DispositionOfInfo)
// and reads a projection instead; the count may only fall. It matches by
// name, so a same-named field of another type (a startup episode's
// QuarantinedUntil) counts too; the baseline carries it.
func TestDispositionReadRatchet(t *testing.T) {
	root := repoRootForLint(t)
	got := map[string]int{}
	files := 0
	for _, dir := range dispositionReadScanDirs {
		paths, err := filepath.Glob(filepath.Join(root, dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range paths {
			base := filepath.Base(p)
			if strings.HasSuffix(base, "_test.go") || dir == "internal/session" && strings.HasPrefix(base, "disposition") {
				continue
			}
			f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", p, err)
			}
			files++
			if n := dispositionRawReads(f); n > 0 {
				got[filepath.ToSlash(filepath.Join(dir, base))] = n
			}
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
		case *ast.IndexExpr:
			if lit, ok := x.Index.(*ast.BasicLit); ok && lit.Kind == token.STRING && !targets[x] {
				if key, err := strconv.Unquote(lit.Value); err == nil && slices.Contains(dispositionMetaKeys, key) {
					n++
				}
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
