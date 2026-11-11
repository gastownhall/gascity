package nogo_test

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
)

const (
	bundleRecordPath = "golangci-lint-bundle.txt"
	bundleGenerator  = "scripts/nogo-bundle-record.sh"
	gofumptModule    = "mvdan.cc/gofumpt"
)

// supportModules are the direct requirements of tools/nogo/go.mod that no
// golangci-lint bundle row covers, each with its reason. They are never
// equality-checked against the bundle: TestModuleRequirementsAgreeWithRoot is
// the only rule on them.
var supportModules = map[string]string{
	"golang.org/x/mod":                "the guard tests parse go.mod and compare versions with it; golangci-lint's own x/mod version does not matter",
	"golang.org/x/tools":              "the go/analysis framework every nogo analyzer is built on, shared with the gc build",
	"github.com/hashicorp/go-version": "revive's config loader needs it; not an analyzer",
}

var (
	makefileLintVersionRE = regexp.MustCompile(`(?m)^GOLANGCI_LINT_VERSION[ \t]*:=[ \t]*(\S+)[ \t]*$`)
	goDepsModuleRE        = regexp.MustCompile(`(?s)go_deps\.module\((.*?)\)`)
	starlarkPathRE        = regexp.MustCompile(`(?m)^\s*path\s*=\s*"([^"]+)"`)
	starlarkVersionRE     = regexp.MustCompile(`(?m)^\s*version\s*=\s*"([^"]+)"`)
)

// bundleRecord is the parsed tools/nogo/golangci-lint-bundle.txt.
type bundleRecord struct {
	version string
	sum     string
	rows    map[string]string
}

// parseBundleRecord reads the record. It reports every malformed line: the
// record is generated, so anything it does not recognize means a hand edit.
func parseBundleRecord(text string) (bundleRecord, []string) {
	rec := bundleRecord{rows: map[string]string{}}
	var problems []string
	headers := 0
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		switch {
		case fields[0] == "golangci-lint":
			headers++
			if headers > 1 {
				problems = append(problems, fmt.Sprintf("%s:%d: a second golangci-lint header line", bundleRecordPath, i+1))
				continue
			}
			if len(fields) != 3 || !strings.HasPrefix(fields[2], "h1:") {
				problems = append(problems, fmt.Sprintf("%s:%d: the header must read `golangci-lint <version> <h1: GoModSum>`, got %q", bundleRecordPath, i+1, line))
				continue
			}
			rec.version, rec.sum = fields[1], fields[2]
		case len(fields) == 2 && strings.HasPrefix(fields[1], "v"):
			if _, dup := rec.rows[fields[0]]; dup {
				problems = append(problems, fmt.Sprintf("%s:%d: duplicate row for %s", bundleRecordPath, i+1, fields[0]))
				continue
			}
			rec.rows[fields[0]] = fields[1]
		default:
			problems = append(problems, fmt.Sprintf("%s:%d: unrecognized line %q (the record is generated: `<module> <version>` rows, a header, and # comments only)", bundleRecordPath, i+1, line))
		}
	}
	if headers == 0 {
		problems = append(problems, bundleRecordPath+": no `golangci-lint <version> <GoModSum>` header line")
	}
	return rec, problems
}

// gofumptVersionInModuleBazel returns the version of the go_deps.module block
// that pins mvdan.cc/gofumpt in MODULE.bazel.
func gofumptVersionInModuleBazel(moduleBazel string) (string, bool) {
	for _, m := range goDepsModuleRE.FindAllStringSubmatch(moduleBazel, -1) {
		path := starlarkPathRE.FindStringSubmatch(m[1])
		if path == nil || path[1] != gofumptModule {
			continue
		}
		version := starlarkVersionRE.FindStringSubmatch(m[1])
		if version == nil {
			return "", false
		}
		return version[1], true
	}
	return "", false
}

// checkBundle compares the tools/nogo analyzer pins with the golangci-lint
// bundle record, the Makefile pin and the MODULE.bazel gofumpt pin. It returns
// every disagreement, never just the first, and each names the file to edit
// and the regeneration command.
func checkBundle(makefile, record, nogoMod, moduleBazel string) []string {
	var problems []string

	m := makefileLintVersionRE.FindStringSubmatch(makefile)
	if m == nil {
		return []string{"Makefile: no `GOLANGCI_LINT_VERSION := <version>` line; restore it (the guard anchors on it)"}
	}
	want := m[1]
	regen := fmt.Sprintf("regenerate with: %s %s", bundleGenerator, want)

	rec, recProblems := parseBundleRecord(record)
	problems = append(problems, recProblems...)
	if rec.version != "" && rec.version != want {
		problems = append(problems, fmt.Sprintf("%s is for golangci-lint %s but the Makefile pins GOLANGCI_LINT_VERSION := %s: %s (rewrites %s), or revert the Makefile", bundleRecordPath, rec.version, want, regen, bundleRecordPath))
	}

	gomod, err := modfile.Parse("tools/nogo/go.mod", []byte(nogoMod), nil)
	if err != nil {
		return append(problems, fmt.Sprintf("tools/nogo/go.mod: %v", err))
	}
	required := map[string]string{}
	var direct []string
	for _, r := range gomod.Require {
		required[r.Mod.Path] = r.Mod.Version
		if !r.Indirect {
			direct = append(direct, r.Mod.Path)
		}
	}

	rows := make([]string, 0, len(rec.rows))
	for mod := range rec.rows {
		rows = append(rows, mod)
	}
	sort.Strings(rows)
	for _, mod := range rows {
		if mod == gofumptModule {
			continue
		}
		have, ok := required[mod]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s has a row for %s but tools/nogo/go.mod does not require it: edit tools/nogo/go.mod (go mod edit -require=%s@%s), or %s", bundleRecordPath, mod, mod, rec.rows[mod], regen))
			continue
		}
		if have != rec.rows[mod] {
			problems = append(problems, fmt.Sprintf("tools/nogo/go.mod requires %s %s; the golangci-lint %s bundle has %s: edit tools/nogo/go.mod (go mod edit -require=%s@%s, then go mod tidy in tools/nogo), or %s", mod, have, want, rec.rows[mod], mod, rec.rows[mod], regen))
		}
	}

	sort.Strings(direct)
	for _, mod := range direct {
		if _, ok := rec.rows[mod]; ok {
			continue
		}
		if _, ok := supportModules[mod]; ok {
			continue
		}
		problems = append(problems, fmt.Sprintf("tools/nogo/go.mod directly requires %s, which has no row in %s and is not a support module: if it is a golangci-lint analyzer add it to the generator's module list in %s and %s; otherwise add it to supportModules in tools/nogo/bundle_test.go with a one-line reason", mod, bundleRecordPath, bundleGenerator, regen))
	}

	bazelVersion, found := gofumptVersionInModuleBazel(moduleBazel)
	switch {
	case !found:
		problems = append(problems, fmt.Sprintf("MODULE.bazel: no go_deps.module(path = %q, version = ...) block; restore it (gofumpt is not in go.mod, so MODULE.bazel is where its version lives)", gofumptModule))
	case rec.rows[gofumptModule] == "":
		problems = append(problems, fmt.Sprintf("%s has no %s row: %s", bundleRecordPath, gofumptModule, regen))
	case bazelVersion != rec.rows[gofumptModule]:
		problems = append(problems, fmt.Sprintf("MODULE.bazel pins %s %s; the golangci-lint %s bundle has %s: edit the go_deps.module block in MODULE.bazel (version and sum, from `go mod download -json %s@%s`), or %s", gofumptModule, bazelVersion, want, rec.rows[gofumptModule], gofumptModule, rec.rows[gofumptModule], regen))
	}
	return problems
}

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

// TestAnalyzerPinsMatchGolangciLintBundle keeps tools/nogo/go.mod at the
// analyzer versions the golangci-lint release pinned in the Makefile compiles
// in. nogo is the only Go linter the Linux gates run and the Mac lane runs real
// golangci-lint, so a pin that trails the bundle passes here and fails there.
func TestAnalyzerPinsMatchGolangciLintBundle(t *testing.T) {
	problems := checkBundle(
		readRepoFile(t, "../../Makefile"),
		readRepoFile(t, bundleRecordPath),
		readRepoFile(t, "go.mod"),
		readRepoFile(t, "../../MODULE.bazel"),
	)
	for _, p := range problems {
		t.Error(p)
	}
}

const (
	fixtureMakefile = "GOLANGCI_LINT_VERSION := 2.14.0\n"
	fixtureRecord   = `# generated
golangci-lint 2.14.0 h1:amHimCzNDP1GfVno3hIiNssjvqXmuW+CPZAHOdR4cIc=
github.com/mgechev/revive v1.17.0
github.com/kisielk/errcheck v1.20.0
mvdan.cc/gofumpt v0.12.0
`
	fixtureGoMod = `module example.com/nogo

go 1.27.3

require (
	github.com/hashicorp/go-version v1.9.0
	github.com/kisielk/errcheck v1.20.0
	github.com/mgechev/revive v1.17.0
	golang.org/x/mod v0.41.0
	golang.org/x/tools v0.51.0
)

require github.com/fatih/color v1.19.0 // indirect
`
	fixtureModuleBazel = `go_deps.module(
    path = "mvdan.cc/gofumpt",
    sum = "h1:xxx",
    version = "v0.12.0",
)
go_deps.module(
    indirect = True,
    path = "github.com/google/go-cmp",
    sum = "h1:yyy",
    version = "v0.7.0",
)
`
)

// TestBundleGuardCatchesDrift runs the comparison over small fixtures: a clean
// one and one mutant per way the pins can drift, each of which must be reported
// with the file to edit.
func TestBundleGuardCatchesDrift(t *testing.T) {
	if got := checkBundle(fixtureMakefile, fixtureRecord, fixtureGoMod, fixtureModuleBazel); len(got) != 0 {
		t.Fatalf("clean fixture reported problems: %v", got)
	}
	cases := []struct {
		name     string
		makefile string
		record   string
		gomod    string
		bazel    string
		want     []string
	}{
		{
			name:  "analyzer pin trails the bundle",
			gomod: strings.Replace(fixtureGoMod, "revive v1.17.0", "revive v1.15.0", 1),
			want:  []string{"tools/nogo/go.mod requires github.com/mgechev/revive v1.15.0", "bundle has v1.17.0", "scripts/nogo-bundle-record.sh 2.14.0"},
		},
		{
			name:     "Makefile pin moves without the record",
			makefile: "GOLANGCI_LINT_VERSION := 2.15.0\n",
			want:     []string{"golangci-lint-bundle.txt is for golangci-lint 2.14.0", "Makefile pins GOLANGCI_LINT_VERSION := 2.15.0", "scripts/nogo-bundle-record.sh 2.15.0"},
		},
		{
			name:  "gofumpt pin in MODULE.bazel trails the bundle",
			bazel: strings.Replace(fixtureModuleBazel, "v0.12.0", "v0.11.0", 1),
			want:  []string{"MODULE.bazel pins mvdan.cc/gofumpt v0.11.0", "bundle has v0.12.0"},
		},
		{
			name:  "unlisted direct requirement",
			gomod: strings.Replace(fixtureGoMod, "require (\n", "require (\n\texample.com/newlint v1.0.0\n", 1),
			want:  []string{"directly requires example.com/newlint", "supportModules"},
		},
		{
			name:     "Makefile loses its pin line",
			makefile: "GOLANGCI_LINT_VERSION = 2.14.0 # not the anchored form\n",
			want:     []string{"Makefile: no `GOLANGCI_LINT_VERSION := <version>` line"},
		},
		{
			name:   "duplicate row",
			record: fixtureRecord + "github.com/mgechev/revive v1.17.0\n",
			want:   []string{"duplicate row for github.com/mgechev/revive"},
		},
		{
			name:   "unknown line",
			record: fixtureRecord + "tolerate everything\n",
			want:   []string{"unrecognized line"},
		},
		{
			name:   "no header",
			record: "github.com/mgechev/revive v1.17.0\nmvdan.cc/gofumpt v0.12.0\n",
			want:   []string{"no `golangci-lint <version> <GoModSum>` header line"},
		},
		{
			name:   "row for a module go.mod does not require",
			record: fixtureRecord + "example.com/gone v1.0.0\n",
			want:   []string{"row for example.com/gone but tools/nogo/go.mod does not require it"},
		},
		{
			name:  "gofumpt block missing from MODULE.bazel",
			bazel: "go_deps.module(path = \"example.com/other\", version = \"v1.0.0\")\n",
			want:  []string{"MODULE.bazel: no go_deps.module(path = \"mvdan.cc/gofumpt\""},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			makefile, record, gomod, bazel := fixtureMakefile, fixtureRecord, fixtureGoMod, fixtureModuleBazel
			if tc.makefile != "" {
				makefile = tc.makefile
			}
			if tc.record != "" {
				record = tc.record
			}
			if tc.gomod != "" {
				gomod = tc.gomod
			}
			if tc.bazel != "" {
				bazel = tc.bazel
			}
			got := strings.Join(checkBundle(makefile, record, gomod, bazel), "\n")
			if got == "" {
				t.Fatal("no problem reported")
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("report lacks %q:\n%s", w, got)
				}
			}
		})
	}
}

// TestBundleGuardReportsEveryMismatch pins that one run lists all drifted
// pins, not just the first.
func TestBundleGuardReportsEveryMismatch(t *testing.T) {
	gomod := strings.Replace(fixtureGoMod, "revive v1.17.0", "revive v1.15.0", 1)
	gomod = strings.Replace(gomod, "errcheck v1.20.0", "errcheck v1.10.0", 1)
	got := checkBundle(fixtureMakefile, fixtureRecord, gomod, fixtureModuleBazel)
	if len(got) != 2 {
		t.Fatalf("want 2 problems (revive and errcheck), got %d: %v", len(got), got)
	}
}
