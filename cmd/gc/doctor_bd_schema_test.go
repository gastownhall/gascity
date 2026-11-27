package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// bdSchemaReadForTest is what reading one scope's database answers in a
// doctor test: a gap (found), nothing (at the linked schema), or an error.
type bdSchemaReadForTest struct {
	gap   bdSchemaGap
	found bool
	err   error
}

// newBdSchemaDoctorCheckForTest builds the check over fixed scopes of a
// single-store city whose databases read as reads says, by scope label.
func newBdSchemaDoctorCheckForTest(scopes []bdSchemaScope, reads map[string]bdSchemaReadForTest) *bdSchemaDoctorCheck {
	return &bdSchemaDoctorCheck{
		scopes: func(string, []config.Rig) []bdSchemaScope { return scopes },
		city:   func(cityPath string, _ *config.City) bdSchemaCity { return bdSchemaCity{path: cityPath} },
		deps: bdSchemaDeps{
			read: func(_ string, scope bdSchemaScope) (bdSchemaGap, bool, error) {
				r := reads[scope.label]
				r.gap.scope = scope
				return r.gap, r.found, r.err
			},
			binary:  func(string, string) string { return "/opt/beads/bd" },
			version: func(string) string { return "bd version 1.3.4" },
		},
	}
}

// TestBdSchemaDoctorCheck pins gc doctor's bd-schema check: OK when every
// database it reads is at the linked library's schema (the at-schema case
// stays quiet), an advisory Warning naming each database that is not, with
// the operator's command in the details, and an unreadable database listed as
// not checked either way, without claiming a schema it never read. It never
// offers a fix: migrating an existing database is the consent gc does not
// give.
func TestBdSchemaDoctorCheck(t *testing.T) {
	main, ignored := beads.PinnedSchemaCursors()
	linked := proxyendpoint.Cursors{Main: main, Ignored: ignored}
	atSchema := bdSchemaReadForTest{}
	behind := bdSchemaReadForTest{found: true, gap: bdSchemaGap{
		database: "hq", endpoint: "127.0.0.1:3307",
		observed: proxyendpoint.Cursors{Main: main - 5, Ignored: ignored - 4},
		linked:   linked, direction: beads.ProxiedSkewDirBehind,
	}}
	ahead := bdSchemaReadForTest{found: true, gap: bdSchemaGap{
		database: "mp", endpoint: "127.0.0.1:3307",
		observed: proxyendpoint.Cursors{Main: main + 2, Ignored: ignored},
		linked:   linked, direction: beads.ProxiedSkewDirAhead,
	}}
	unreadable := bdSchemaReadForTest{err: errors.New("connection refused")}
	city := bdSchemaScope{label: "city", root: "/city"}
	rigA := bdSchemaScope{label: `rig "a"`, root: "/a", rig: "a"}
	rigB := bdSchemaScope{label: `rig "b"`, root: "/b", rig: "b"}
	ctx := &doctor.CheckContext{CityPath: "/city"}

	t.Run("skipped where gc runs no Dolt", func(t *testing.T) {
		r := newBdSchemaDoctorCheck(nil, nil, true).Run(ctx)
		if r.Status != doctor.StatusOK || !strings.Contains(r.Message, "skipped") {
			t.Fatalf("got %+v, want OK skipped", r)
		}
	})
	t.Run("no bd database on a Dolt server", func(t *testing.T) {
		r := newBdSchemaDoctorCheckForTest(nil, nil).Run(ctx)
		if r.Status != doctor.StatusOK {
			t.Fatalf("got %+v, want OK", r)
		}
	})
	t.Run("every database at the linked schema", func(t *testing.T) {
		r := newBdSchemaDoctorCheckForTest([]bdSchemaScope{city, rigA}, map[string]bdSchemaReadForTest{
			"city": atSchema, `rig "a"`: atSchema,
		}).Run(ctx)
		if r.Status != doctor.StatusOK || !strings.Contains(r.Message, "2 of 2") || len(r.Details) != 0 {
			t.Fatalf("got %+v, want OK over 2 of 2 with no details", r)
		}
	})
	t.Run("no database readable", func(t *testing.T) {
		r := newBdSchemaDoctorCheckForTest([]bdSchemaScope{city, rigA}, map[string]bdSchemaReadForTest{
			"city": unreadable, `rig "a"`: unreadable,
		}).Run(ctx)
		if r.Status != doctor.StatusOK || !strings.Contains(r.Message, "none of 2") || strings.Contains(r.Message, "each at") || len(r.Details) != 2 {
			t.Fatalf("got %+v, want an OK that says nothing was checked, listing both", r)
		}
	})
	t.Run("behind, ahead and unreadable databases", func(t *testing.T) {
		c := newBdSchemaDoctorCheckForTest([]bdSchemaScope{city, rigA, rigB}, map[string]bdSchemaReadForTest{
			"city": behind, `rig "a"`: ahead, `rig "b"`: unreadable,
		})
		r := c.Run(ctx)
		if r.Status != doctor.StatusWarning || r.Severity != doctor.SeverityAdvisory {
			t.Fatalf("got status %v severity %v, want an advisory warning: %+v", r.Status, r.Severity, r)
		}
		want := fmt.Sprintf("v%d/%d", main, ignored)
		if !strings.Contains(r.Message, "2 bd database(s) not at "+want) {
			t.Fatalf("message = %q", r.Message)
		}
		if len(r.Details) != 3 {
			t.Fatalf("details = %q, want the two notices and the unreadable database", r.Details)
		}
		if !strings.Contains(r.Details[0], `bd schema: city database "hq"`) || !strings.HasSuffix(r.Details[0], "gc bd --city /city migrate schema") {
			t.Fatalf("first detail is not the city's notice with its command: %q", r.Details[0])
		}
		if !strings.Contains(r.Details[1], `bd schema: rig "a" database "mp"`) || !strings.Contains(r.Details[1], "ahead of "+want) {
			t.Fatalf("second detail is not rig a's ahead notice: %q", r.Details[1])
		}
		if r.Details[2] != `rig "b": not checked: connection refused` {
			t.Fatalf("third detail = %q", r.Details[2])
		}
		if r.FixHint == "" || c.CanFix() {
			t.Fatalf("want an operator fix hint and no automatic fix: hint %q canFix %v", r.FixHint, c.CanFix())
		}
		if !strings.Contains(r.FixHint, "both migration lanes") {
			t.Fatalf("fix hint %q does not say a rerun verifies both lanes", r.FixHint)
		}
	})
	t.Run("configured migrate command", func(t *testing.T) {
		c := newBdSchemaDoctorCheckForTest([]bdSchemaScope{city}, map[string]bdSchemaReadForTest{"city": behind})
		c.city = func(cityPath string, _ *config.City) bdSchemaCity {
			return bdSchemaCity{path: cityPath, migrateCommand: "cd {{shq .ScopeDir}} && BEADS_DIR={{shq .BeadsDir}} /opt/migrator/bd migrate schema"}
		}
		r := c.Run(ctx)
		if r.Status != doctor.StatusWarning || len(r.Details) != 1 {
			t.Fatalf("got %+v, want one warning detail", r)
		}
		if !strings.HasSuffix(r.Details[0], ": cd /city && BEADS_DIR=/city/.beads /opt/migrator/bd migrate schema (from a shell with no other BEADS_DOLT_* variables set)") {
			t.Fatalf("detail does not name the configured command rendered for the city: %q", r.Details[0])
		}
		if !strings.Contains(r.Details[0], "The migrator must move both lanes") {
			t.Fatalf("detail lacks the two-lane caution: %q", r.Details[0])
		}
	})
}
