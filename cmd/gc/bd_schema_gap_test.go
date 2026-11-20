package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// linkedCursorsForTest is the schema of the beads library linked into this
// test binary, which every gap is measured against.
func linkedCursorsForTest() proxyendpoint.Cursors {
	main, ignored := beads.PinnedSchemaCursors()
	return proxyendpoint.Cursors{Main: main, Ignored: ignored}
}

// TestBdSchemaGapFromReportComparesBothLanesAsTheLibraryReadsThem pins what
// counts as a gap: either lane behind or ahead of the linked library's, main
// first, with the ignored lane read the way the library reads it (a lane the
// live schema does not corroborate counts at its floor). A database at the
// linked schema has none, which is what keeps gc start and gc doctor quiet
// about it.
func TestBdSchemaGapFromReportComparesBothLanesAsTheLibraryReadsThem(t *testing.T) {
	linked := linkedCursorsForTest()
	scope := bdSchemaScope{label: `rig "my-project"`, root: "/srv/my-project", rig: "my-project"}
	target := contract.DoltConnectionTarget{Host: "127.0.0.1", Port: "3307", Database: "mp"}
	for _, tc := range []struct {
		name          string
		report        proxyendpoint.CursorReport
		wantFound     bool
		wantDirection string
		wantObserved  proxyendpoint.Cursors
		wantNote      bool
	}{
		{
			name:   "at the linked schema",
			report: proxyendpoint.CursorReport{Cursors: linked},
		},
		{
			name:          "main lane behind",
			report:        proxyendpoint.CursorReport{Cursors: proxyendpoint.Cursors{Main: linked.Main - 5, Ignored: linked.Ignored - 4}},
			wantFound:     true,
			wantDirection: beads.ProxiedSkewDirBehind,
			wantObserved:  proxyendpoint.Cursors{Main: linked.Main - 5, Ignored: linked.Ignored - 4},
		},
		{
			name:          "ignored lane behind alone",
			report:        proxyendpoint.CursorReport{Cursors: proxyendpoint.Cursors{Main: linked.Main, Ignored: linked.Ignored - 1}},
			wantFound:     true,
			wantDirection: beads.ProxiedSkewDirBehind,
			wantObserved:  proxyendpoint.Cursors{Main: linked.Main, Ignored: linked.Ignored - 1},
		},
		{
			name:          "main lane ahead",
			report:        proxyendpoint.CursorReport{Cursors: proxyendpoint.Cursors{Main: linked.Main + 3, Ignored: linked.Ignored}},
			wantFound:     true,
			wantDirection: beads.ProxiedSkewDirAhead,
			wantObserved:  proxyendpoint.Cursors{Main: linked.Main + 3, Ignored: linked.Ignored},
		},
		{
			name: "ignored lane on disk at the linked schema, but not corroborated",
			report: proxyendpoint.CursorReport{
				Cursors: linked,
				Reality: proxyendpoint.CursorReality{Limited: true, Floor: linked.Ignored - 5, Missing: "events"},
			},
			wantFound:     true,
			wantDirection: beads.ProxiedSkewDirBehind,
			wantObserved:  proxyendpoint.Cursors{Main: linked.Main, Ignored: linked.Ignored - 5},
			wantNote:      true,
		},
		{
			name: "main lane behind, ignored lane clamped to no lower than it reads",
			report: proxyendpoint.CursorReport{
				Cursors: proxyendpoint.Cursors{Main: linked.Main - 1, Ignored: linked.Ignored - 5},
				Reality: proxyendpoint.CursorReality{Limited: true, Floor: linked.Ignored - 2, Missing: "events"},
			},
			wantFound:     true,
			wantDirection: beads.ProxiedSkewDirBehind,
			wantObserved:  proxyendpoint.Cursors{Main: linked.Main - 1, Ignored: linked.Ignored - 5},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gap, found := bdSchemaGapFromReport(scope, target, tc.report)
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v (gap %+v)", found, tc.wantFound, gap)
			}
			if !found {
				return
			}
			if gap.direction != tc.wantDirection || gap.observed != tc.wantObserved || gap.linked != linked {
				t.Fatalf("gap = %+v, want direction %s, observed %+v, linked %+v", gap, tc.wantDirection, tc.wantObserved, linked)
			}
			if gap.database != "mp" || gap.endpoint != "127.0.0.1:3307" || gap.host != "127.0.0.1" || gap.port != "3307" || gap.socket != "" || gap.scope != scope {
				t.Fatalf("gap locates the wrong database: %+v", gap)
			}
			if (gap.realityNote != "") != tc.wantNote {
				t.Fatalf("realityNote = %q, want a note %v", gap.realityNote, tc.wantNote)
			}
		})
	}
}

// TestBdSchemaGapFromReportKeepsASocketEndpoint pins that a database served
// on a Unix socket is located by that socket, with no TCP address, which is
// what a configured migrate command reads as .Socket, .Host and .Port.
func TestBdSchemaGapFromReportKeepsASocketEndpoint(t *testing.T) {
	linked := linkedCursorsForTest()
	target := contract.DoltConnectionTarget{Socket: "/run/dolt.sock", Database: "hq"}
	report := proxyendpoint.CursorReport{Cursors: proxyendpoint.Cursors{Main: linked.Main - 1, Ignored: linked.Ignored}}
	gap, found := bdSchemaGapFromReport(bdSchemaScope{label: "city", root: "/city"}, target, report)
	if !found {
		t.Fatalf("a behind database read as current")
	}
	if gap.endpoint != "/run/dolt.sock" || gap.socket != "/run/dolt.sock" || gap.host != "" || gap.port != "" {
		t.Fatalf("socket endpoint not kept: %+v", gap)
	}
}

// TestBdSchemaGapNoticeNamesTheGapAndTheOperatorCommand pins the operator's
// line: it names the scope, the database and its server, both schemas, what
// the pinned bd reports, why gc leaves the database alone, and the exact
// command an operator runs. Where gc bd forwards `migrate` the command is gc
// bd, which projects the scope's own store and server; a split city's gc bd
// refuses it, so there it is the pinned bd with BEADS_DIR pinned, quoted for
// the shell. An ahead database gets the other remedy, and a city that opted
// in to beads.allow_schema_behind_migrate is told its own setting consents.
func TestBdSchemaGapNoticeNamesTheGapAndTheOperatorCommand(t *testing.T) {
	linked := linkedCursorsForTest()
	rigGap := bdSchemaGap{
		scope:     bdSchemaScope{label: `rig "my-project"`, root: "/srv/my-project", rig: "my-project"},
		database:  "mp",
		endpoint:  "127.0.0.1:3307",
		observed:  proxyendpoint.Cursors{Main: linked.Main - 5, Ignored: linked.Ignored - 4},
		linked:    linked,
		direction: beads.ProxiedSkewDirBehind,
	}
	single := bdSchemaCity{path: "/srv/my-city"}
	split := bdSchemaCity{path: "/srv/my-city", split: true}
	observed := fmt.Sprintf("v%d/%d", linked.Main-5, linked.Ignored-4)
	want := fmt.Sprintf("v%d/%d", linked.Main, linked.Ignored)

	t.Run("behind, rig of a single-store city", func(t *testing.T) {
		got := rigGap.notice(single, "/opt/beads/bd", "bd version 1.3.4 (abc123def456)")
		for _, part := range []string{
			`bd schema: rig "my-project" database "mp" on 127.0.0.1:3307 is at schema ` + observed,
			"behind " + want + ", the schema of the beads library linked into gc",
			`the pinned bd reports "bd version 1.3.4 (abc123def456)"`,
			"gc migrates a database only while bootstrapping it",
			"Once every client of this database runs a bd that knows " + want + ", an operator migrates it with a bd at that schema: gc bd --city /srv/my-city --rig my-project migrate schema",
		} {
			if !strings.Contains(got, part) {
				t.Fatalf("notice missing %q:\n%s", part, got)
			}
		}
		if strings.Contains(got, "--force") || strings.Contains(got, "BEADS_DOLT_") {
			t.Fatalf("a single-store database with no remote needs neither --force nor an env warning:\n%s", got)
		}
	})
	t.Run("behind, city scope", func(t *testing.T) {
		cityGap := rigGap
		cityGap.scope = bdSchemaScope{label: "city", root: "/srv/my-city"}
		got := cityGap.notice(single, "", "")
		if !strings.HasSuffix(got, "an operator migrates it with a bd at that schema: gc bd --city /srv/my-city migrate schema") {
			t.Fatalf("expected the city's gc bd command:\n%s", got)
		}
		if strings.Contains(got, "pinned bd reports") {
			t.Fatalf("named a version the pinned bd did not report:\n%s", got)
		}
	})
	t.Run("behind, rig of a split city", func(t *testing.T) {
		spaced := rigGap
		spaced.scope.root = "/srv/my project"
		got := spaced.notice(split, "/opt/my bd/bd", "")
		if !strings.Contains(got, "cd '/srv/my project' && BEADS_DIR='/srv/my project'/.beads '/opt/my bd/bd' migrate schema (from a shell with no BEADS_DOLT_* variables set)") {
			t.Fatalf("split city's command is not the pinned bd with BEADS_DIR pinned, quoted for the shell:\n%s", got)
		}
	})
	t.Run("behind, remote-backed", func(t *testing.T) {
		remote := rigGap
		remote.hasRemote = true
		got := remote.notice(single, "/opt/beads/bd", "")
		if !strings.Contains(got, "--force") || !strings.Contains(got, "bd dolt push") {
			t.Fatalf("a remote-backed database's notice must say what bd will ask:\n%s", got)
		}
	})
	t.Run("behind, city opted in to schema-behind migration", func(t *testing.T) {
		optedIn := single
		optedIn.allowBehind = true
		got := rigGap.notice(optedIn, "/opt/beads/bd", "")
		if !strings.Contains(got, "beads.allow_schema_behind_migrate is on") || strings.Contains(got, "gc migrates a database only while bootstrapping it") {
			t.Fatalf("an opted-in city's notice must say its own setting consents:\n%s", got)
		}
	})
	t.Run("ahead", func(t *testing.T) {
		ahead := rigGap
		ahead.observed = proxyendpoint.Cursors{Main: linked.Main + 3, Ignored: linked.Ignored}
		ahead.direction = beads.ProxiedSkewDirAhead
		got := ahead.notice(single, "/opt/beads/bd", "")
		if !strings.Contains(got, "ahead of "+want) || !strings.Contains(got, "run a gc built on that schema") {
			t.Fatalf("ahead notice must tell the operator to bring gc up to the database:\n%s", got)
		}
		if strings.Contains(got, "migrate schema") {
			t.Fatalf("ahead notice must not prescribe a migration:\n%s", got)
		}
	})
}

// TestBdSchemaGapNoticeCautionsThatAMigratorMovesBothLanes pins the caution
// every behind notice carries ahead of its command: a migrator has to bring
// both migration lanes to the linked schema, and gc doctor's bd-schema check
// reads both afterwards. An ahead notice prescribes no migration, so it has
// none.
func TestBdSchemaGapNoticeCautionsThatAMigratorMovesBothLanes(t *testing.T) {
	linked := linkedCursorsForTest()
	want := fmt.Sprintf("v%d/%d", linked.Main, linked.Ignored)
	caution := "The migrator must move both lanes, main and ignored, to " + want +
		": one that moves only the main lane leaves it behind, where a bd may refuse to open it. Afterwards gc doctor --check bd-schema reads both lanes."
	gap := bdSchemaGap{
		scope:     bdSchemaScope{label: "city", root: "/srv/my-city"},
		database:  "hq",
		endpoint:  "127.0.0.1:3307",
		observed:  proxyendpoint.Cursors{Main: linked.Main - 5, Ignored: linked.Ignored - 4},
		linked:    linked,
		direction: beads.ProxiedSkewDirBehind,
	}
	for name, city := range map[string]bdSchemaCity{
		"default command":            {path: "/srv/my-city"},
		"configured command":         {path: "/srv/my-city", migrateCommand: "/opt/migrator/bd migrate schema"},
		"opted in to behind migrate": {path: "/srv/my-city", allowBehind: true},
	} {
		t.Run(name, func(t *testing.T) {
			got := gap.notice(city, "/opt/beads/bd", "")
			if !strings.Contains(got, caution) {
				t.Fatalf("notice lacks the two-lane caution %q:\n%s", caution, got)
			}
			if strings.Index(got, caution) > strings.LastIndex(got, "migrate schema") {
				t.Fatalf("the caution must come before the command, which ends the sentence an operator copies:\n%s", got)
			}
		})
	}
	ahead := gap
	ahead.observed = proxyendpoint.Cursors{Main: linked.Main + 1, Ignored: linked.Ignored}
	ahead.direction = beads.ProxiedSkewDirAhead
	if got := ahead.notice(bdSchemaCity{path: "/srv/my-city"}, "", ""); strings.Contains(got, "both lanes") {
		t.Fatalf("an ahead notice prescribes no migration, so it needs no caution:\n%s", got)
	}
}

// TestBdSchemaGapNoticeNamesTheConfiguredMigrateCommand pins [beads]
// migrate_command: when it is set, every behind notice names it, rendered
// for that database, in place of gc's default command, in a single-store and
// a split city alike, followed by the note to run it with no other
// BEADS_DOLT_* variables set (it runs bd itself, which takes them over the
// scope's own). A
// template that fails to render (load validation keeps one from getting
// here) falls back to the default command and says why, rather than printing
// nothing an operator can run.
func TestBdSchemaGapNoticeNamesTheConfiguredMigrateCommand(t *testing.T) {
	linked := linkedCursorsForTest()
	tmpl := "cd {{shq .ScopeDir}} && BEADS_DIR={{shq .BeadsDir}} BEADS_DOLT_SERVER_HOST={{.Host}} BEADS_DOLT_SERVER_PORT={{.Port}} " +
		"/opt/migrator/bd migrate schema # {{.City}} {{.Rig}} {{.Database}} {{.Socket}} {{.Bd}}"
	rigGap := bdSchemaGap{
		scope:     bdSchemaScope{label: `rig "my-project"`, root: "/srv/my project", rig: "my-project"},
		database:  "mp",
		endpoint:  "127.0.0.1:3307",
		host:      "127.0.0.1",
		port:      "3307",
		observed:  proxyendpoint.Cursors{Main: linked.Main - 5, Ignored: linked.Ignored - 4},
		linked:    linked,
		direction: beads.ProxiedSkewDirBehind,
	}
	const shellNote = " (from a shell with no other BEADS_DOLT_* variables set)"
	wantRig := "cd '/srv/my project' && BEADS_DIR='/srv/my project/.beads' BEADS_DOLT_SERVER_HOST=127.0.0.1 BEADS_DOLT_SERVER_PORT=3307 " +
		"/opt/migrator/bd migrate schema # /srv/my-city my-project mp  /opt/beads/bd"

	t.Run("rig of a single-store city", func(t *testing.T) {
		got := rigGap.notice(bdSchemaCity{path: "/srv/my-city", migrateCommand: tmpl}, "/opt/beads/bd", "")
		if !strings.HasSuffix(got, "an operator migrates it with a bd at that schema: "+wantRig+shellNote) {
			t.Fatalf("notice does not end with the configured command rendered for the rig:\n%s", got)
		}
		if strings.Contains(got, "gc bd --city") {
			t.Fatalf("notice still names gc's default command:\n%s", got)
		}
	})
	t.Run("rig of a split city", func(t *testing.T) {
		got := rigGap.notice(bdSchemaCity{path: "/srv/my-city", split: true, migrateCommand: tmpl}, "/opt/beads/bd", "")
		if !strings.HasSuffix(got, ": "+wantRig+shellNote) {
			t.Fatalf("a split city's notice does not end with the configured command:\n%s", got)
		}
	})
	t.Run("city scope on a socket, unresolved pinned bd", func(t *testing.T) {
		cityGap := rigGap
		cityGap.scope = bdSchemaScope{label: "city", root: "/srv/my-city"}
		cityGap.host, cityGap.port, cityGap.socket = "", "", "/run/dolt.sock"
		got := cityGap.notice(bdSchemaCity{path: "/srv/my-city", migrateCommand: tmpl}, "", "")
		want := "cd /srv/my-city && BEADS_DIR=/srv/my-city/.beads BEADS_DOLT_SERVER_HOST= BEADS_DOLT_SERVER_PORT= " +
			"/opt/migrator/bd migrate schema # /srv/my-city  mp /run/dolt.sock bd"
		if !strings.HasSuffix(got, ": "+want+shellNote) {
			t.Fatalf("notice does not end with the configured command rendered for the city:\n%s", got)
		}
	})
	t.Run("opted in to behind migrate", func(t *testing.T) {
		got := rigGap.notice(bdSchemaCity{path: "/srv/my-city", allowBehind: true, migrateCommand: tmpl}, "/opt/beads/bd", "")
		if !strings.Contains(got, "To migrate it first") || !strings.HasSuffix(got, ": "+wantRig+shellNote) {
			t.Fatalf("an opted-in city's notice does not name the configured command:\n%s", got)
		}
	})
	t.Run("remote-backed", func(t *testing.T) {
		remote := rigGap
		remote.hasRemote = true
		got := remote.notice(bdSchemaCity{path: "/srv/my-city", migrateCommand: tmpl}, "/opt/beads/bd", "")
		if !strings.Contains(got, wantRig+shellNote+" (it has a Dolt remote") {
			t.Fatalf("a remote-backed database's notice must follow the configured command with bd's --force note:\n%s", got)
		}
	})
	t.Run("template that fails to render", func(t *testing.T) {
		got := rigGap.notice(bdSchemaCity{path: "/srv/my-city", migrateCommand: "bd {{.Nope}}"}, "/opt/beads/bd", "")
		if !strings.Contains(got, "gc bd --city /srv/my-city --rig my-project migrate schema (beads.migrate_command did not render:") {
			t.Fatalf("a failed render must fall back to the default command and say why:\n%s", got)
		}
	})
}

// TestNewBdSchemaCityCarriesTheConfiguredMigrateCommand pins the hand-off
// from config to the notices gc start, gc reload and gc doctor print: the
// city's [beads] migrate_command reaches collectBdSchemaReport, which renders
// it into each notice, and a city whose config did not load (gc doctor still
// runs the check then) gets gc's default command.
func TestNewBdSchemaCityCarriesTheConfiguredMigrateCommand(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{Beads: config.BeadsConfig{MigrateCommand: "/opt/migrator/bd --db {{.Database}} migrate schema"}}
	city := newBdSchemaCity(cityPath, cfg)
	if city.migrateCommand != cfg.Beads.MigrateCommand {
		t.Fatalf("migrateCommand = %q, want %q", city.migrateCommand, cfg.Beads.MigrateCommand)
	}
	linked := linkedCursorsForTest()
	deps := bdSchemaDeps{
		read: func(_ string, scope bdSchemaScope) (bdSchemaGap, bool, error) {
			return bdSchemaGap{
				scope: scope, database: "hq", endpoint: "127.0.0.1:3307", host: "127.0.0.1", port: "3307",
				observed: proxyendpoint.Cursors{Main: linked.Main - 1, Ignored: linked.Ignored},
				linked:   linked, direction: beads.ProxiedSkewDirBehind,
			}, true, nil
		},
		binary:  func(string, string) string { return "/opt/beads/bd" },
		version: func(string) string { return "" },
	}
	if got := newBdSchemaCity(cityPath, nil); got.migrateCommand != "" {
		t.Fatalf("a city whose config did not load got migrate command %q", got.migrateCommand)
	}
	report := collectBdSchemaReport(city, []bdSchemaScope{{label: "city", root: cityPath}}, deps)
	if len(report.notices) != 1 || !strings.HasSuffix(report.notices[0], ": /opt/migrator/bd --db hq migrate schema (from a shell with no other BEADS_DOLT_* variables set)") {
		t.Fatalf("notices = %q, want the configured command rendered for hq", report.notices)
	}
}

// TestCollectBdSchemaReportReportsGapsAndUnreadableDatabases pins the pass
// gc start, gc reload and gc doctor share: a notice per gap in scope order,
// nothing for a database at the linked schema, unreadable databases kept
// apart (gc start and gc reload skip them, gc doctor lists them), and one
// `bd version` per distinct pinned bd however many scopes share it.
func TestCollectBdSchemaReportReportsGapsAndUnreadableDatabases(t *testing.T) {
	linked := linkedCursorsForTest()
	scopes := []bdSchemaScope{
		{label: "city", root: "/city"},
		{label: `rig "a"`, root: "/a", rig: "a"},
		{label: `rig "b"`, root: "/b", rig: "b"},
		{label: `rig "c"`, root: "/c", rig: "c"},
	}
	behindGap := func(scope bdSchemaScope) bdSchemaGap {
		return bdSchemaGap{
			scope: scope, database: "db", endpoint: "127.0.0.1:3307",
			observed: proxyendpoint.Cursors{Main: linked.Main - 1, Ignored: linked.Ignored},
			linked:   linked, direction: beads.ProxiedSkewDirBehind,
		}
	}
	versionCalls := map[string]int{}
	deps := bdSchemaDeps{
		read: func(_ string, scope bdSchemaScope) (bdSchemaGap, bool, error) {
			switch scope.label {
			case `rig "a"`:
				return bdSchemaGap{}, false, nil
			case `rig "b"`:
				return bdSchemaGap{}, false, errors.New("dial tcp 127.0.0.1:3307: connect: connection refused")
			default:
				return behindGap(scope), true, nil
			}
		},
		binary: func(string, string) string { return "/opt/beads/bd" },
		version: func(bdPath string) string {
			versionCalls[bdPath]++
			return "bd version 1.3.4"
		},
	}

	report := collectBdSchemaReport(bdSchemaCity{path: "/city"}, scopes, deps)
	if len(report.gaps) != 2 || len(report.notices) != 2 {
		t.Fatalf("want gaps for city and rig c, got %+v", report)
	}
	if !strings.Contains(report.notices[0], "bd schema: city ") || !strings.Contains(report.notices[1], `bd schema: rig "c" `) {
		t.Fatalf("notices out of scope order:\n%s", strings.Join(report.notices, "\n"))
	}
	if len(report.unreadable) != 1 || report.unreadable[0].label != `rig "b"` {
		t.Fatalf("unreadable = %+v, want rig b", report.unreadable)
	}
	if versionCalls["/opt/beads/bd"] != 1 {
		t.Fatalf("bd version ran %d times for one pinned bd, want 1", versionCalls["/opt/beads/bd"])
	}
}

// TestBdSchemaScopeEligibleOnlyForServerModeBdScopes pins which scopes gc
// start, gc reload and gc doctor read: the bd scopes whose init adopts a
// database on a Dolt server, the only ones the migration step ever ran on. An
// embedded store is bd's own to migrate or refuse, a proxied one gates its
// schema in its own lane, a DoltLite store has no Dolt schema of this kind, a
// store bound elsewhere is not gc's to read, and a file store has no schema,
// so none of them is read.
func TestBdSchemaScopeEligibleOnlyForServerModeBdScopes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		backend  string
		metadata string
		want     bool
	}{
		{"bd scope in server mode", "", "", `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`, true},
		{"bd scope in embedded mode", "", "", `{"database":"dolt","backend":"dolt","dolt_mode":"embedded","dolt_database":"hq"}`, false},
		{"bd scope in proxied-server mode", "", "", `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"hq"}`, false},
		{"bd scope on DoltLite", "", "doltlite", `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`, false},
		{"bd scope bound to a store gc does not serve", "", "", `{"backend":"dolt","storage_endpoint":"opaque-remote","storage_database":"work"}`, false},
		{"file store", "file", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_BEADS", tc.provider)
			t.Setenv("GC_BEADS_SCOPE_ROOT", "")
			t.Setenv("GC_BEADS_BACKEND", tc.backend)
			cityPath := t.TempDir()
			if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
				t.Fatal(err)
			}
			if tc.metadata != "" {
				if err := os.WriteFile(scopeMetadataJSONPath(cityPath), []byte(tc.metadata), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := bdSchemaScopeEligible(cityPath, cityPath); got != tc.want {
				t.Fatalf("bdSchemaScopeEligible = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestScopeReadinessErrorNamesASchemaGapAsUnserved pins how a failed
// post-init readiness wait is classified: a scope whose database is not at the
// linked library's schema is one the pinned bd refused for it, which the
// lifecycle leaves unserved; any other failure, including one where the
// database cannot be read, stays the plain readiness error it always was.
func TestScopeReadinessErrorNamesASchemaGapAsUnserved(t *testing.T) {
	readinessErr := errors.New("bd list: database is at v74, binary knows up to v69")
	for _, tc := range []struct {
		name         string
		found        bool
		readErr      error
		wantUnserved bool
	}{
		{name: "schema gap", found: true, wantUnserved: true},
		{name: "at the linked schema"},
		{name: "database unreadable", readErr: errors.New("connection refused")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := liveBdSchemaDeps
			liveBdSchemaDeps.read = func(string, bdSchemaScope) (bdSchemaGap, bool, error) {
				return bdSchemaGap{}, tc.found, tc.readErr
			}
			t.Cleanup(func() { liveBdSchemaDeps = old })

			err := scopeReadinessError("/city", "/rig", readinessErr)
			var unserved *bdSchemaUnservedError
			if got := errors.As(err, &unserved); got != tc.wantUnserved {
				t.Fatalf("unserved = %v, want %v (err %v)", got, tc.wantUnserved, err)
			}
			if !errors.Is(err, readinessErr) || !strings.Contains(err.Error(), "waiting for initialized bead scope readiness") {
				t.Fatalf("err = %v, want it to wrap the readiness failure", err)
			}
		})
	}
}

// useBdSchemaNoticesForTest makes every caller of bdSchemaNoticesFor see
// notices, and counts the calls.
func useBdSchemaNoticesForTest(t *testing.T, notices ...string) *int {
	t.Helper()
	calls := 0
	old := bdSchemaNoticesFor
	bdSchemaNoticesFor = func(string, *config.City) []string {
		calls++
		return notices
	}
	t.Cleanup(func() { bdSchemaNoticesFor = old })
	return &calls
}

// startBeadsLifecycleFixtureForTest is a city whose bead lifecycle runs with
// no store of its own to start (the file provider, GC_DOLT=skip), with one rig
// per name in rigs.
func startBeadsLifecycleFixtureForTest(t *testing.T, rigs ...string) (string, *config.City) {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	cityPath := t.TempDir()
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeDoltState(cityPath, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      4406,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	for _, name := range rigs {
		cfg.Rigs = append(cfg.Rigs, config.Rig{Name: name, Path: t.TempDir()})
	}
	return cityPath, cfg
}

// TestStartBeadsLifecycleNamesBdSchemaGapsWhetherOrNotItSucceeds pins that the
// boot lifecycle (gc start's, and the supervisor's city start) writes the bd
// schema notices to its stderr when it succeeds and when it fails: a city
// store the pinned bd cannot open is one way it fails, and then the notice is
// what says why and what to run.
func TestStartBeadsLifecycleNamesBdSchemaGapsWhetherOrNotItSucceeds(t *testing.T) {
	const notice = "bd schema: rig \"my-project\" database \"mp\" …"
	for _, tc := range []struct {
		name    string
		initErr error
	}{
		{"lifecycle succeeds", nil},
		{"lifecycle fails", errors.New("exec beads init: boom")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, cfg := startBeadsLifecycleFixtureForTest(t, "my-project")
			calls := useBdSchemaNoticesForTest(t, notice)
			old := startBeadsLifecycleInitRig
			startBeadsLifecycleInitRig = func(string, string, string) error { return tc.initErr }
			t.Cleanup(func() { startBeadsLifecycleInitRig = old })

			var stderr bytes.Buffer
			err := startBeadsLifecycle(cityPath, "test-city", cfg, &stderr)
			if (err != nil) != (tc.initErr != nil) {
				t.Fatalf("startBeadsLifecycle err = %v, want error %v", err, tc.initErr != nil)
			}
			if *calls != 1 || !strings.Contains(stderr.String(), "warning: "+notice+"\n") {
				t.Fatalf("notices asked %d time(s), stderr:\n%s", *calls, stderr.String())
			}
		})
	}
}

// TestStartBeadsLifecycleLeavesARigItCannotServeUnserved pins that a rig whose
// database the pinned bd could not open because of its schema does not take
// the city down: the lifecycle says so, leaves that rig unserved, and goes on
// to the next rig. Any other rig init failure still fails the lifecycle.
func TestStartBeadsLifecycleLeavesARigItCannotServeUnserved(t *testing.T) {
	cityPath, cfg := startBeadsLifecycleFixtureForTest(t, "skewed", "healthy")
	useBdSchemaNoticesForTest(t)
	var inited []string
	old := startBeadsLifecycleInitRig
	startBeadsLifecycleInitRig = func(_ string, dir, _ string) error {
		inited = append(inited, dir)
		if dir == cfg.Rigs[0].Path {
			return &bdSchemaUnservedError{err: errors.New("bd list: database is at v74, binary knows up to v69")}
		}
		return nil
	}
	t.Cleanup(func() { startBeadsLifecycleInitRig = old })

	var stderr bytes.Buffer
	if err := startBeadsLifecycle(cityPath, "test-city", cfg, &stderr); err != nil {
		t.Fatalf("startBeadsLifecycle: %v", err)
	}
	if len(inited) != 2 || inited[1] != cfg.Rigs[1].Path {
		t.Fatalf("rigs initialized = %v, want both, the healthy one after the skewed one", inited)
	}
	if !strings.Contains(stderr.String(), `init rig "skewed" beads:`) || !strings.Contains(stderr.String(), "leaving the rig unserved until its database is migrated") {
		t.Fatalf("stderr does not say the skewed rig is left unserved:\n%s", stderr.String())
	}
}

// TestCityRuntimeReloadRepliesWithBdSchemaNotices pins that gc reload's reply
// carries the bd schema notices as warnings, which `gc reload` prints, on a
// reload that applies and on one whose bead lifecycle fails.
func TestCityRuntimeReloadRepliesWithBdSchemaNotices(t *testing.T) {
	const notice = "bd schema: rig \"my-project\" database \"mp\" …"
	for _, tc := range []struct {
		name         string
		lifecycleErr error
		wantOutcome  reloadOutcome
	}{
		{"reload applies", nil, reloadOutcomeApplied},
		{"bead lifecycle fails", errors.New("init city beads: waiting for initialized bead scope readiness: boom"), reloadOutcomeFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			tomlPath := filepath.Join(cityPath, "city.toml")
			writeCityRuntimeConfig(t, tomlPath, "fake")
			cfg, err := config.Load(osFS{}, tomlPath)
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			sp := runtime.NewFake()
			var stdout, stderr bytes.Buffer
			cr := newTestCityRuntime(t, CityRuntimeParams{
				CityPath:  cityPath,
				CityName:  "test-city",
				TomlPath:  tomlPath,
				LogPrefix: "gc reload",
				Cfg:       cfg,
				SP:        sp,
				BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
					return DesiredStateResult{State: map[string]TemplateParams{}}
				},
				Dops:   newDrainOps(sp),
				Rec:    events.Discard,
				Stdout: &stdout,
				Stderr: &stderr,
			})
			cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
			cs.cityBeadStore = beads.NewMemStore()
			cr.setControllerState(cs)
			cr.sessionDrains = newDrainTracker()

			useBdSchemaNoticesForTest(t, notice)
			prevStart := cityRuntimeStartBeadsLifecycle
			cityRuntimeStartBeadsLifecycle = func(string, string, *config.City, cityStoreRecord, io.Writer) error {
				return tc.lifecycleErr
			}
			t.Cleanup(func() { cityRuntimeStartBeadsLifecycle = prevStart })

			data := []byte("[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n\n[daemon]\nshutdown_timeout = \"1s\"\n")
			if err := os.WriteFile(tomlPath, data, 0o644); err != nil {
				t.Fatalf("write config: %v", err)
			}
			lastProviderName := "fake"
			reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceManual)
			if reply.Outcome != tc.wantOutcome {
				t.Fatalf("reply.Outcome = %q, want %q (error %q)", reply.Outcome, tc.wantOutcome, reply.Error)
			}
			if !warningsContain(reply.Warnings, notice) {
				t.Fatalf("reply.Warnings = %v, want the bd schema notice", reply.Warnings)
			}
		})
	}
}

// TestGcStartPrintsBdSchemaWarnings pins gc start's own line for each bd
// schema notice: under the supervisor the lifecycle's go to its log, so the
// command prints them too.
func TestGcStartPrintsBdSchemaWarnings(t *testing.T) {
	const notice = "bd schema: rig \"my-project\" database \"mp\" …"
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	useBdSchemaNoticesForTest(t, notice)

	var stderr bytes.Buffer
	printBdSchemaStartWarnings(cityPath, &stderr)
	if got, want := stderr.String(), "gc start: warning: "+notice+"\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}
