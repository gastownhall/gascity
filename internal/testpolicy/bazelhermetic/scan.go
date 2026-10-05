// Package bazelhermetic finds Go tests whose results depend on inputs Bazel
// does not declare, so a remotely cached PASS for them may no longer be true.
//
// Bazel keys a test result on its declared inputs (sources, data, the test
// environment it forwards, the platform). A test that dials the internet,
// compares a fixed calendar date with the wall clock, or shares a writable
// host path with other actions reads state that is not part of that key. Such
// a test must either be made hermetic or carry a tag that keeps its result out
// of the cache (`external`, `no-cache`, or `no-remote-cache`). The ledger in
// test/bazel-hermeticity.toml records both decisions, and
// tools/bazel/hermetic_tags.py writes the ledger's tags into BUILD.bazel.
//
// The scan is syntactic and import-aware (no type checking): it recognizes
// calls through each file's own import names, and only literal arguments.
// It is a ratchet against new, obvious cases, not a proof of hermeticity;
// runtime enforcement (network-less execution) covers indirect paths.
package bazelhermetic

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Signal names one class of undeclared test input.
type Signal string

// Signals the scanner reports.
const (
	// SignalDNS is a DNS lookup through package net.
	SignalDNS Signal = "dns"
	// SignalExternalURL is a literal non-loopback URL or host:port passed
	// directly to a network client call or to a subprocess.
	SignalExternalURL Signal = "external-url"
	// SignalNetworkCLI is a subprocess whose only purpose is network access
	// (gh, curl, wget, ssh, docker, kubectl, `go get`, `go mod download`).
	SignalNetworkCLI Signal = "network-cli"
	// SignalWallClock relates a fixed calendar date to the wall clock
	// (time.Since(date), date.Before(time.Now()), ...), the shape of an
	// expiry or "before date X" check.
	SignalWallClock Signal = "wall-clock"
	// SignalSharedTmp is a filesystem or unix-socket write to a fixed literal
	// /tmp path, which concurrent actions on one host share.
	SignalSharedTmp Signal = "shared-tmp"
)

// AllSignals lists every signal in reporting order.
var AllSignals = []Signal{SignalDNS, SignalExternalURL, SignalNetworkCLI, SignalWallClock, SignalSharedTmp}

// Finding is one occurrence of a signal in a Go test file.
type Finding struct {
	Package string // repository-relative package directory, forward slashes
	File    string // repository-relative file path
	Line    int
	Signal  Signal
	Detail  string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: %s: %s", f.File, f.Line, f.Signal, f.Detail)
}

// skipDirs are never scanned: VCS and tool state, generated Bazel trees,
// vendored code, and Go testdata (fixtures the go tool never compiles).
var skipDirs = map[string]bool{
	".git": true, ".claude": true, ".worktrees": true, ".gc": true, ".beads": true,
	"node_modules": true, "third_party": true, "testdata": true, "vendor": true,
}

// skipDir returns fs.SkipDir for directories Scan never enters.
func skipDir(name string, entry fs.DirEntry) error {
	base := entry.Name()
	if name != "." && (skipDirs[base] || strings.HasPrefix(base, "bazel-") || strings.HasPrefix(base, ".")) {
		return fs.SkipDir
	}
	return nil
}

// Scan walks sourceFS and returns every finding, sorted by file and line.
// Only *_test.go files are scanned. A test file importing waiverclock is a
// finding; package code importing it is not: it may only build
// waiverclock.Expiry values, and a clock read in package code decides its
// consumers' tests, which a signal on the importing package cannot name.
func Scan(sourceFS fs.FS) ([]Finding, error) {
	var findings []Finding
	fset := token.NewFileSet()
	err := fs.WalkDir(sourceFS, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return skipDir(name, entry)
		}
		if !strings.HasSuffix(name, "_test.go") {
			return nil
		}
		src, err := fs.ReadFile(sourceFS, name)
		if err != nil {
			return fmt.Errorf("reading %s: %w", name, err)
		}
		file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", name, err)
		}
		findings = append(findings, datedWaiverImports(fset, name, file)...)
		findings = append(findings, scanFile(fset, name, file)...)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scanning test sources: %w", err)
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})
	return findings, nil
}

// WaiverClockImport is the package every dated test-policy ledger asks for
// "today" (TESTING.md "Waiver expiry clocks"). A test importing it makes its
// package's test outcome a function of the calendar date.
const WaiverClockImport = "github.com/gastownhall/gascity/internal/testpolicy/waiverclock"

func datedWaiverImports(fset *token.FileSet, name string, file *ast.File) []Finding {
	var out []Finding
	for _, spec := range file.Imports {
		if importPath, err := strconv.Unquote(spec.Path.Value); err == nil && importPath == WaiverClockImport {
			out = append(out, Finding{
				Package: path.Dir(name),
				File:    name,
				Line:    fset.Position(spec.Pos()).Line,
				Signal:  SignalWallClock,
				Detail:  "imports waiverclock: dated waivers expire with the calendar",
			})
		}
	}
	return out
}

type fileScanner struct {
	fset    *token.FileSet
	name    string
	pkgDir  string
	imports map[string]string // local name -> import path
	out     []Finding
}

func scanFile(fset *token.FileSet, name string, file *ast.File) []Finding {
	s := &fileScanner{fset: fset, name: name, pkgDir: path.Dir(name), imports: map[string]string{}}
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		local := path.Base(importPath)
		if spec.Name != nil {
			local = spec.Name.Name
		}
		s.imports[local] = importPath
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		s.scanFunc(fn)
	}
	return s.out
}

func (s *fileScanner) report(pos token.Pos, signal Signal, detail string) {
	s.out = append(s.out, Finding{
		Package: s.pkgDir,
		File:    s.name,
		Line:    s.fset.Position(pos).Line,
		Signal:  signal,
		Detail:  detail,
	})
}

// pkgCall returns the import path and function name of a pkg.Func call made
// through one of the file's imports, or "" when the call is anything else.
func (s *fileScanner) pkgCall(call *ast.CallExpr) (string, string) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", ""
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", ""
	}
	importPath, ok := s.imports[id.Name]
	if !ok {
		return "", ""
	}
	return importPath, sel.Sel.Name
}

func (s *fileScanner) scanFunc(fn *ast.FuncDecl) {
	clock := s.collectClockIdents(fn.Body)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := node.X.(*ast.Ident); ok && s.imports[id.Name] == "net" && node.Sel.Name == "DefaultResolver" {
				s.report(node.Pos(), SignalDNS, "net.DefaultResolver")
			}
		case *ast.CallExpr:
			s.checkWallClock(node, clock)
			importPath, name := s.pkgCall(node)
			switch importPath {
			case "net":
				s.checkNet(node, name)
			case "net/http":
				s.checkHTTP(node, name)
			case "crypto/tls":
				if strings.HasPrefix(name, "Dial") {
					s.checkAddressArgs(node, "tls."+name, node.Args)
				}
			case "os/exec":
				s.checkExec(node, name)
			case "os":
				s.checkOSPath(node, name)
			}
		}
		return true
	})
}

// clockIdents records, per function, the local names bound to a fixed
// calendar date and to the current wall-clock time.
type clockIdents struct {
	fixed map[string]string // name -> description of the fixed date
	now   map[string]bool
}

func (s *fileScanner) collectClockIdents(body *ast.BlockStmt) clockIdents {
	clock := clockIdents{fixed: map[string]string{}, now: map[string]bool{}}
	bind := func(lhs []ast.Expr, rhs []ast.Expr) {
		if len(rhs) != 1 || len(lhs) == 0 {
			return
		}
		id, ok := lhs[0].(*ast.Ident)
		if !ok || id.Name == "_" {
			return
		}
		if desc, ok := s.fixedDate(rhs[0], clock); ok {
			clock.fixed[id.Name] = desc
		} else if s.isNow(rhs[0], clock) {
			clock.now[id.Name] = true
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			bind(node.Lhs, node.Rhs)
		case *ast.ValueSpec:
			lhs := make([]ast.Expr, len(node.Names))
			for i, name := range node.Names {
				lhs[i] = name
			}
			bind(lhs, node.Values)
		}
		return true
	})
	return clock
}

// timeMethodsKeepingInstant are time.Time methods whose result is still
// derived from the receiver's instant.
var timeMethodsKeepingInstant = map[string]bool{
	"UTC": true, "Local": true, "In": true, "Truncate": true, "Round": true, "Add": true, "AddDate": true,
}

// fixedDate reports whether expr is a fixed calendar date: time.Date with a
// literal year, time.Parse of a literal date, a name bound to one, or a
// method chain on one.
func (s *fileScanner) fixedDate(expr ast.Expr, clock clockIdents) (string, bool) {
	switch e := expr.(type) {
	case *ast.Ident:
		desc, ok := clock.fixed[e.Name]
		return desc, ok
	case *ast.CallExpr:
		if importPath, name := s.pkgCall(e); importPath == "time" {
			switch name {
			case "Date":
				if len(e.Args) > 0 && isIntLiteral(e.Args[0]) {
					return "time.Date(" + literalText(e.Args[0]) + ", ...)", true
				}
			case "Parse", "ParseInLocation":
				if len(e.Args) > 1 {
					if v, ok := stringLiteral(e.Args[1]); ok && calendarDate.MatchString(v) {
						return "time." + name + "(..., " + strconv.Quote(v) + ")", true
					}
				}
			}
			return "", false
		}
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok && timeMethodsKeepingInstant[sel.Sel.Name] {
			return s.fixedDate(sel.X, clock)
		}
	}
	return "", false
}

// isNow reports whether expr is the current wall-clock time.
func (s *fileScanner) isNow(expr ast.Expr, clock clockIdents) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		return clock.now[e.Name]
	case *ast.CallExpr:
		if importPath, name := s.pkgCall(e); importPath == "time" {
			return name == "Now"
		}
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok && timeMethodsKeepingInstant[sel.Sel.Name] {
			return s.isNow(sel.X, clock)
		}
	}
	return false
}

// timeComparisons are time.Time methods that relate two instants.
var timeComparisons = map[string]bool{"Before": true, "After": true, "Sub": true, "Equal": true, "Compare": true}

// checkWallClock reports a fixed calendar date related to the wall clock:
// time.Since(fixed), time.Until(fixed), or fixed.Before(now) and friends.
// Such a result changes with the date the test runs on, which is not an
// input Bazel keys the cached result on.
func (s *fileScanner) checkWallClock(call *ast.CallExpr, clock clockIdents) {
	if importPath, name := s.pkgCall(call); importPath == "time" && (name == "Since" || name == "Until") {
		if len(call.Args) == 1 {
			if desc, ok := s.fixedDate(call.Args[0], clock); ok {
				s.report(call.Pos(), SignalWallClock, fmt.Sprintf("time.%s(%s) depends on the date the test runs", name, desc))
			}
		}
		return
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !timeComparisons[sel.Sel.Name] || len(call.Args) != 1 {
		return
	}
	if desc, ok := s.fixedDate(sel.X, clock); ok && s.isNow(call.Args[0], clock) {
		s.report(call.Pos(), SignalWallClock, fmt.Sprintf("%s.%s(now) depends on the date the test runs", desc, sel.Sel.Name))
		return
	}
	if desc, ok := s.fixedDate(call.Args[0], clock); ok && s.isNow(sel.X, clock) {
		s.report(call.Pos(), SignalWallClock, fmt.Sprintf("now.%s(%s) depends on the date the test runs", sel.Sel.Name, desc))
	}
}

var calendarDate = regexp.MustCompile(`^(19|20)\d\d-\d\d-\d\d`)

var dnsFuncs = map[string]bool{
	"LookupAddr": true, "LookupCNAME": true, "LookupHost": true, "LookupIP": true,
	"LookupMX": true, "LookupNS": true, "LookupPort": true, "LookupSRV": true, "LookupTXT": true,
}

func (s *fileScanner) checkNet(call *ast.CallExpr, name string) {
	switch {
	case dnsFuncs[name]:
		s.report(call.Pos(), SignalDNS, "net."+name)
	case strings.HasPrefix(name, "Dial"):
		s.checkAddressArgs(call, "net."+name, call.Args)
	case name == "Listen" || name == "ListenUnix":
		if len(call.Args) == 2 {
			if network, ok := stringLiteral(call.Args[0]); ok && strings.HasPrefix(network, "unix") {
				s.checkTmpArg(call, "net."+name, call.Args[1])
			}
		}
	}
}

var httpClientFuncs = map[string]bool{
	"Get": true, "Head": true, "Post": true, "PostForm": true,
	"NewRequest": true, "NewRequestWithContext": true,
}

func (s *fileScanner) checkHTTP(call *ast.CallExpr, name string) {
	if httpClientFuncs[name] {
		s.checkAddressArgs(call, "http."+name, call.Args)
	}
}

// checkAddressArgs reports literal arguments naming an external host.
func (s *fileScanner) checkAddressArgs(call *ast.CallExpr, callee string, args []ast.Expr) {
	for _, arg := range args {
		v, ok := stringLiteral(arg)
		if !ok {
			continue
		}
		if host, external := externalHost(v); external {
			s.report(call.Pos(), SignalExternalURL, fmt.Sprintf("%s(%q) reaches %s", callee, v, host))
			return
		}
	}
}

// networkCLIs are programs a test can only run for their network access.
var networkCLIs = map[string]bool{
	"gh": true, "curl": true, "wget": true, "ssh": true, "scp": true, "rsync": true,
	"docker": true, "podman": true, "kubectl": true, "helm": true,
	"npm": true, "npx": true, "pip": true, "pip3": true,
}

func (s *fileScanner) checkExec(call *ast.CallExpr, name string) {
	var args []ast.Expr
	switch name {
	case "Command":
		args = call.Args
	case "CommandContext":
		if len(call.Args) > 0 {
			args = call.Args[1:]
		}
	case "LookPath":
		args = call.Args
	default:
		return
	}
	if len(args) == 0 {
		return
	}
	program, ok := stringLiteral(args[0])
	if !ok {
		return
	}
	base := path.Base(program)
	switch {
	case networkCLIs[base]:
		s.report(call.Pos(), SignalNetworkCLI, fmt.Sprintf("exec.%s(%q)", name, program))
		return
	case base == "go" && len(args) > 1:
		sub, _ := stringLiteral(args[1])
		third := ""
		if len(args) > 2 {
			third, _ = stringLiteral(args[2])
		}
		if sub == "get" || (sub == "mod" && third == "download") {
			s.report(call.Pos(), SignalNetworkCLI, fmt.Sprintf("exec.%s(\"go\", %q) downloads modules", name, strings.TrimSpace(sub+" "+third)))
			return
		}
	}
	s.checkAddressArgs(call, "exec."+name, args[1:])
}

// osWriteFuncs take the path they create or modify as their first argument.
// os.MkdirTemp and os.CreateTemp are absent: a unique name under /tmp is not
// state another action can observe.
var osWriteFuncs = map[string]bool{
	"Create": true, "Mkdir": true, "MkdirAll": true,
	"OpenFile": true, "WriteFile": true, "Remove": true, "RemoveAll": true,
	"Rename": true, "Symlink": true, "Link": true, "Chmod": true,
}

func (s *fileScanner) checkOSPath(call *ast.CallExpr, name string) {
	if !osWriteFuncs[name] || len(call.Args) == 0 {
		return
	}
	arg := call.Args[0]
	if name == "Symlink" || name == "Link" || name == "Rename" {
		if len(call.Args) < 2 {
			return
		}
		arg = call.Args[1]
	}
	s.checkTmpArg(call, "os."+name, arg)
}

func (s *fileScanner) checkTmpArg(call *ast.CallExpr, callee string, arg ast.Expr) {
	// filepath.Join("/tmp", ...) and path.Join("/tmp", ...) count too.
	if inner, ok := arg.(*ast.CallExpr); ok {
		if importPath, name := s.pkgCall(inner); (importPath == "path/filepath" || importPath == "path") && name == "Join" && len(inner.Args) > 0 {
			arg = inner.Args[0]
		}
	}
	v, ok := stringLiteral(arg)
	if !ok {
		return
	}
	if v == "/tmp" || strings.HasPrefix(v, "/tmp/") || v == "/var/tmp" || strings.HasPrefix(v, "/var/tmp/") {
		s.report(call.Pos(), SignalSharedTmp, fmt.Sprintf("%s(%q) writes a host path shared between actions", callee, v))
	}
}

// externalHost reports the host a literal URL or host:port names, and whether
// that host is outside the loopback/reserved set a test may use freely.
func externalHost(v string) (string, bool) {
	var host string
	switch {
	case strings.Contains(v, "://"):
		u, err := url.Parse(v)
		if err != nil || u.Scheme == "file" || u.Scheme == "unix" {
			return "", false
		}
		host = u.Hostname()
	case strings.HasPrefix(v, "git@"):
		rest := strings.TrimPrefix(v, "git@")
		host, _, _ = strings.Cut(rest, ":")
	default:
		h, _, err := net.SplitHostPort(v)
		if err != nil {
			return "", false
		}
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || reservedHost(host) {
		return host, false
	}
	// A bare word without a dot ("db", "server") is a placeholder or a
	// test-local name, not a public host.
	if !strings.Contains(host, ".") && net.ParseIP(host) == nil {
		return host, false
	}
	return host, true
}

func reservedHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	for _, suffix := range []string{".invalid", ".test", ".example", ".local", ".internal"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	for _, domain := range []string{"example.com", "example.org", "example.net"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	return false
}

func stringLiteral(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return v, true
}

func isIntLiteral(expr ast.Expr) bool {
	lit, ok := expr.(*ast.BasicLit)
	return ok && lit.Kind == token.INT
}

func literalText(expr ast.Expr) string {
	if lit, ok := expr.(*ast.BasicLit); ok {
		return lit.Value
	}
	return "?"
}
