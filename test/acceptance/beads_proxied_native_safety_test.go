//go:build acceptance_a

// The proxied-native lane's safety rows.
//
// P2-14 measures what the lane makes cheaper; P2-15 measures how it behaves when
// the proxy moves. This file is about the two things that would make the lane
// unshippable however fast it is:
//
//	gc must never spawn a Dolt server for a scope bd owns, and
//	gc must never let the linked library migrate a database bd owns.
//
// Both are claims about something NOT happening, which is the hardest kind of
// claim to test honestly: a process that was never started leaves no trace, so
// "no trace found" is equally true of a correct gc and of a test looking in the
// wrong place. Each row here is therefore paired with a positive control — an
// instrument shown to record the thing it is claiming the absence of — and the
// assertion is the intersection of two observations rather than the absence of
// one.
//
// The third row is a forward pin rather than a safety property: beads latches
// GIT_AUTHOR_NAME/EMAIL onto the store instance AT OPEN, so the author of every
// Dolt commit a proxied-window handle ever makes is decided by the env map PR2
// projects. PR2 writes nothing through that handle on any product path, which is
// exactly why the pin has to exist now: PR3's authorship is already determined by
// code that has landed, and a beads bump that moved that read would otherwise
// surface as mis-attributed commits in PR3 rather than as a failure here.
package acceptance_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/gastownhall/gascity/internal/beads"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// proxiedSentinelEnv is proxiedEnvRecordingBD with a recording `dolt` in front of
// the pinned one.
//
// The sentinel goes AHEAD of the symlink directory proxiedEnv builds, so it is
// the `dolt` every PATH lookup finds — including bd's own, which resolves dolt
// once at proxy start and passes the result to its proxy child as `--dolt-bin`.
// That is what puts bd's Dolt spawns in the log beside gc's, which is what makes
// the no-spawn assertion falsifiable.
//
// The hermetic provider doubles stay at index 0, as everywhere else in this
// suite.
func proxiedSentinelEnv(t *testing.T, bdPath, doltPath string) (*helpers.Env, *helpers.RecordingBD, *helpers.SentinelDolt) {
	t.Helper()
	env, calls := proxiedEnvRecordingBD(t, bdPath, doltPath)
	sentinel := helpers.NewSentinelDolt(t, doltPath)
	entries := filepath.SplitList(env.Get("PATH"))
	path := append([]string{entries[0], sentinel.Dir}, entries[1:]...)
	return env.With("PATH", strings.Join(path, string(os.PathListSeparator))), calls, sentinel
}

// proxiedScopeSQL opens a session against bd's proxy data port, the way the
// admission probe does: loopback, user root, the scope's own database.
//
// It is the only way these rows can reach the two cursor tables at all. bd offers
// no SQL escape hatch, the library cannot be asked for a raw query, and opening
// the data dir directly is impossible while the server holds its lock. The
// session is deliberately a single non-idle connection so it leaves nothing behind
// for bd's idle watcher to count.
func proxiedScopeSQL(t *testing.T, scopeRoot string) *sql.DB {
	t.Helper()
	root := proxiedScopeProxyRoot(t, scopeRoot)
	var record struct {
		Port int `json:"port"`
	}
	readJSONFile(t, filepath.Join(root, "proxy.pid"), &record)
	if record.Port <= 0 {
		t.Fatalf("the proxy record under %s names no data port", root)
	}
	var metadata proxiedBeadsMetadata
	readJSONFile(t, filepath.Join(scopeRoot, ".beads", "metadata.json"), &metadata)
	database := strings.TrimSpace(metadata.DoltDatabase)
	if database == "" {
		t.Fatalf("%s names no dolt_database", scopeRoot)
	}

	cfg := mysql.NewConfig()
	cfg.User = "root"
	cfg.Net = "tcp"
	cfg.Addr = "127.0.0.1:" + strconv.Itoa(record.Port)
	cfg.DBName = database
	cfg.Timeout = 15 * time.Second
	cfg.ReadTimeout = 30 * time.Second
	cfg.WriteTimeout = 30 * time.Second
	cfg.AllowNativePasswords = true
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatalf("build a proxy connector: %v", err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(0)
	t.Cleanup(func() { db.Close() }) //nolint:errcheck // test session
	if err := db.PingContext(context.Background()); err != nil {
		db.Close() //nolint:errcheck // best effort
		t.Fatalf("dial bd's proxy on %s: %v", cfg.Addr, err)
	}
	return db
}

// proxiedCursors reads the two schema-migration cursors straight off the database,
// with the same queries the admission probe uses.
func proxiedCursors(t *testing.T, db *sql.DB) (main, ignored int) {
	t.Helper()
	for _, q := range []struct {
		query string
		into  *int
	}{
		{"SELECT COALESCE(MAX(version), 0) FROM schema_migrations", &main},
		{"SELECT COALESCE(MAX(version), 0) FROM ignored_schema_migrations", &ignored},
	} {
		if err := db.QueryRowContext(context.Background(), q.query).Scan(q.into); err != nil {
			t.Fatalf("read a cursor with %q: %v", q.query, err)
		}
	}
	return main, ignored
}

func proxiedExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// proxiedExecAffected is proxiedExec for the statements whose EFFECT is the
// point.
//
// A DELETE that matches nothing succeeds, and a cursor row this test failed to
// remove would leave the lane correctly serving a database that is not actually
// skewed — a row that passes for the wrong reason in the one direction that
// matters.
func proxiedExecAffected(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	res, err := db.ExecContext(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("rows affected for %q: %v", query, err)
	}
	return affected
}

func TestProxiedNativeSafety(t *testing.T) {
	bdPath, doltPath := requireProxiedTooling(t)
	env, bdCalls, sentinel := proxiedSentinelEnv(t, bdPath, doltPath)
	// The two unlock variables bd documents for scripted consent, exported into
	// every gc process this test runs.
	//
	// That is the point of the no-migrate rows: an operator who has set them —
	// legitimately, for their own bd commands — must not thereby hand the linked
	// library permission to migrate a database bd owns. The proxied open window
	// withholds the whole BD_ namespace around the library open, so these are
	// actively UNSET for its duration, and the rows below are what makes that a
	// measurement instead of a comment.
	env = env.With("BD_ALLOW_REMOTE_MIGRATE", "1").With("BD_IGNORE_SCHEMA_SKEW", "1")
	lane := proxiedNativeLaneEnv(env)

	city := helpers.NewCity(t, env)
	cityRoot := city.Dir
	t.Cleanup(func() {
		helpers.RunGC(env, cityRoot, "stop", cityRoot) //nolint:errcheck // best effort
		if leaked := waitForNoDoltProcesses(t, cityRoot, 20*time.Second); len(leaked) > 0 {
			t.Errorf("processes outlived the safety city:\n%s", strings.Join(leaked, "\n"))
		}
	})
	city.InitNoStart("claude")
	assertProxiedScope(t, cityRoot, "the safety city")
	// The lifecycle fixture's two readers (tryAccount, heal) are reused here
	// rather than reimplemented: they already know that a disturbed scope may
	// report no beads-store result at all, which every row below depends on.
	fixture := &proxiedNativeCity{
		env: env, lane: lane, calls: bdCalls, bdPath: bdPath,
		city: city, root: cityRoot, proxyDir: proxiedScopeProxyRoot(t, cityRoot),
	}

	// One `gc doctor --json` in the flag-on lane, before anything is disturbed:
	// the lane has to be serving natively or none of the rows below are about it.
	payload, result := readBeadsStorePayload(t, lane, cityRoot, "the safety city")
	if payload.Store != "NativeDoltStore" {
		t.Fatalf("the safety city is not serving natively, so none of these rows would be about the lane: store=%q %s\n  %s",
			payload.Store, describeProxiedAccount(payload), result.Message)
	}

	t.Run("no-spawn-control", func(t *testing.T) {
		// The instrument, before the claim. Two properties, both required for the
		// next row to mean anything:
		//
		//   1. the sentinel sees gc's OWN dolt children. `gc init` runs
		//      `dolt version` and two `dolt config --global --get` — the identity
		//      preflight — directly, so a log with no gc-ancestored entry at all
		//      would mean the sentinel is not the dolt gc resolves, and the next
		//      row's zero would be zero by construction. (doctor's dolt check also
		//      runs `dolt version`, but through a `timeout` wrapper, so its
		//      immediate parent is timeout and it is not what this counts.)
		//   2. the sentinel sees sql-server spawns. bd started one for this city,
		//      so a log with no server entry would mean the sentinel is not the
		//      dolt BD finds either, and "no gc-spawned server" would again be
		//      vacuous.
		invocations := sentinel.Invocations()
		t.Logf("sentinel recorded %d dolt exec(s):\n%s", len(invocations), sentinel.Describe())

		gcAncestored := sentinel.CountWhere(func(i helpers.SentinelDoltInvocation) bool {
			return i.ParentCommandIs("gc")
		})
		if gcAncestored == 0 {
			t.Fatalf("the sentinel recorded no dolt exec with gc as its parent, so it is not the dolt gc resolves and the no-spawn row below would pass by construction:\n%s",
				sentinel.Describe())
		}
		servers := sentinel.CountWhere(helpers.SentinelDoltInvocation.IsServer)
		if servers == 0 {
			t.Fatalf("the sentinel recorded no `dolt sql-server` at all, so it is not the dolt bd resolves and the no-spawn row below would pass by construction:\n%s",
				sentinel.Describe())
		}
		t.Logf("positive control: %d gc-ancestored dolt exec(s) and %d sql-server spawn(s) are visible to the instrument", gcAncestored, servers)
	})

	t.Run("no-spawn", func(t *testing.T) {
		// The claim. Every Dolt SERVER under this city must have been started by
		// bd's proxy, never by gc — including through the escalation path, which
		// is the one place gc asks for a Dolt child to exist at all. So the row
		// stops the proxy first and then drives the flag-on lane, which spends its
		// one ping, and bd starts a fresh Dolt behind it.
		if out, err := helpers.RunGC(env, cityRoot, "stop", cityRoot); err != nil {
			t.Logf("gc stop: %v\n%s", err, out)
		}
		if leaked := waitForNoDoltProcesses(t, cityRoot, 30*time.Second); len(leaked) > 0 {
			t.Logf("processes still under the city after gc stop:\n%s", strings.Join(leaked, "\n"))
		}

		sentinel.Reset()
		bdCalls.Reset()
		out, err := helpers.RunGC(lane, cityRoot, "status", "--json")
		if err != nil {
			t.Logf("gc status --json after the stop: %v\n%s", err, out)
		}
		// And a doctor pass, so the lane has certainly opened a store and has
		// certainly run its own dolt checks in the same window.
		if _, _, ok := fixture.tryAccount(t, "the restarted safety city"); !ok {
			t.Logf("doctor reported no beads-store result after the restart")
		}

		invocations := sentinel.Invocations()
		t.Logf("after a stop and a flag-on open: %d bd fork(s) (%d ping), %d dolt exec(s):\n%s",
			bdCalls.Count(), bdCalls.Count("ping"), len(invocations), sentinel.Describe())

		// Stated as "only bd may" rather than "gc may not". The stronger form is
		// also the honest one: a Dolt server under this city started by anything
		// that is not bd's proxy child is a second owner for bd's process,
		// whichever binary it turned out to be.
		for _, i := range invocations {
			if !i.IsServer() {
				continue
			}
			if !i.ParentCommandIs("bd") {
				t.Errorf("a Dolt server under a scope bd owns was started by %q, not by bd: ppid=%s parent=%q argv=%v",
					i.ParentCommand(), i.PPID, i.Parent, i.Argv)
			}
		}
		if servers := sentinel.CountWhere(helpers.SentinelDoltInvocation.IsServer); servers == 0 {
			t.Errorf("no Dolt server was started at all in this window, so the row proved nothing: the escalation is supposed to make BD start one:\n%s",
				sentinel.Describe())
		}
	})

	// Everything below needs a served lane again.
	healed := fixture.heal(t, "before the no-migrate rows")
	t.Logf("the safety city is serving natively again at generation %s", healed)

	t.Run("no-migrate-behind-ignored", func(t *testing.T) {
		// A database whose IGNORED lane is behind this binary's pinned cursor.
		//
		// That lane is the one bd's own shared-store migration gate does not
		// consult, which is exactly why gc gates on both: a reader that compared
		// only the main lane would meet a database its linked library would
		// migrate without anyone having asked for it.
		//
		// The instrument is the fork census, not the doctor payload, and the
		// reason is a real finding about bd rather than a convenience. Measured
		// here, three times: the FIRST bd child of any gc command re-applies the
		// removed ignored migration — 25 back to 26 — and it does so WITH or
		// WITHOUT BD_ALLOW_REMOTE_MIGRATE exported. `gc doctor` forks bd before it
		// opens the store (doctorBeadStorePreflight), so by the time the
		// beads-store check runs, the database is healthy again and the payload
		// honestly reports a served lane. The verdict for this direction is
		// therefore not observable through doctor on this host at all.
		//
		// `gc status --json` is observable, because it opens the store BEFORE it
		// forks anything and because on a healthy city in this lane it costs
		// EXACTLY ZERO bd forks (the P2-14 gate). So the measurement is a
		// before/after pair on the same command: skewed must fork, healthy must
		// not. A pair is what makes it attributable — a single "it forked" could
		// be any refusal at all.
		db := proxiedScopeSQL(t, cityRoot)
		beforeMain, beforeIgnored := proxiedCursors(t, db)
		if beforeIgnored == 0 {
			// Not a skip: this row is the only acceptance proof of the
			// ignored-lane consent fence, and it runs in a required job.
			helpers.MissingPrecondition(t, "this database's ignored lane is already at 0; there is no top row to remove, "+
				"so the ignored-lane consent fence cannot be measured")
		}
		t.Cleanup(func() {
			proxiedExec(t, db, "INSERT IGNORE INTO ignored_schema_migrations (version) VALUES (?)", beforeIgnored)
		})

		if affected := proxiedExecAffected(t, db, "DELETE FROM ignored_schema_migrations WHERE version = ?", beforeIgnored); affected != 1 {
			t.Fatalf("DELETE of ignored_schema_migrations version %d affected %d row(s); the row this test is about does not exist as it expects",
				beforeIgnored, affected)
		}
		skewedMain, skewedIgnored := proxiedCursors(t, db)
		if skewedIgnored >= beforeIgnored {
			t.Fatalf("after deleting the top ignored row a fresh read still sees main=%d ignored=%d (was main=%d ignored=%d); the mutation is not visible, so nothing below is about gc",
				skewedMain, skewedIgnored, beforeMain, beforeIgnored)
		}

		bdCalls.Reset()
		if out, err := helpers.RunGC(lane, cityRoot, "status", "--json"); err != nil {
			t.Logf("gc status --json over a skewed database: %v\n%s", err, out)
		}
		skewedForks := bdCalls.Count()
		repairedMain, repairedIgnored := proxiedCursors(t, db)
		t.Logf("ignored lane at %d (pinned %d): gc status --json cost %d bd fork(s); the cursors read main=%d ignored=%d afterwards",
			skewedIgnored, beforeIgnored, skewedForks, repairedMain, repairedIgnored)
		if skewedForks == 0 {
			t.Fatalf("gc served a database whose ignored lane is behind this binary natively and forked nothing; the cursor gate runs BEFORE the library open precisely so this cannot happen:\n%s",
				bdCalls.Describe())
		}

		// The other half of the pair. bd's children have repaired the cursor by
		// now — that is what the log above records — so the identical command on
		// the identical city must be back to zero. Without this the row would
		// pass for a lane that had simply stopped working.
		if repairedIgnored != beforeIgnored || repairedMain != beforeMain {
			t.Fatalf("the cursors are main=%d ignored=%d, want the original main=%d ignored=%d before the control run",
				repairedMain, repairedIgnored, beforeMain, beforeIgnored)
		}
		bdCalls.Reset()
		if out, err := helpers.RunGC(lane, cityRoot, "status", "--json"); err != nil {
			t.Fatalf("gc status --json over the restored database: %v\n%s", err, out)
		}
		healthyForks := bdCalls.Count()
		t.Logf("with the cursors restored: gc status --json cost %d bd fork(s)", healthyForks)
		if healthyForks != 0 {
			t.Errorf("the same command on the restored database still cost %d bd fork(s), so the %d it cost while skewed is not attributable to the skew:\n%s",
				healthyForks, skewedForks, bdCalls.Describe())
		}
	})

	t.Run("no-migrate-ahead-main", func(t *testing.T) {
		// The other direction, and the more dangerous one: a database AHEAD of
		// this binary. Nothing gc can do will make its linked library understand
		// a newer schema, and issuing old-shape SQL against it is how a reader
		// corrupts a store it has no business writing to.
		db := proxiedScopeSQL(t, cityRoot)
		beforeMain, beforeIgnored := proxiedCursors(t, db)
		bogus := beforeMain + 1000
		proxiedExec(t, db, "INSERT INTO schema_migrations (version) VALUES (?)", bogus)
		restored := false
		t.Cleanup(func() {
			if restored {
				return
			}
			proxiedExec(t, db, "DELETE FROM schema_migrations WHERE version = ?", bogus)
		})

		payload, result := readBeadsStorePayloadWith(t, lane, cityRoot, "a database ahead of this binary", "beads-store")
		t.Logf("no-migrate-ahead-main: store=%q %s", payload.Store, describeProxiedAccount(payload))
		assertProxiedSchemaSkewRefusal(t, payload, result, "main", "ahead")

		proxiedExec(t, db, "DELETE FROM schema_migrations WHERE version = ?", bogus)
		restored = true
		if main, ignored := proxiedCursors(t, db); main != beforeMain || ignored != beforeIgnored {
			t.Fatalf("restoring the cursor pair left main=%d ignored=%d, want main=%d ignored=%d", main, ignored, beforeMain, beforeIgnored)
		}
		payload, result = readBeadsStorePayloadWith(t, lane, cityRoot, "the restored database", "beads-store")
		if payload.Store != "NativeDoltStore" {
			t.Fatalf("the lane did not come back once the cursors matched again: store=%q %s\n  %s",
				payload.Store, describeProxiedAccount(payload), result.Message)
		}
	})

	t.Run("author-latched-at-open", func(t *testing.T) {
		// The forward pin for PR3's authorship.
		//
		// beads reads GIT_AUTHOR_NAME/GIT_AUTHOR_EMAIL in applyConfigDefaults at
		// OPEN (v1.3.0 internal/storage/dolt/store.go:1430,:1449,:1455), latches
		// them onto the store instance (:1974-1975), and commitAuthorString
		// (:3170) is what feeds DOLT_COMMIT(..., '--author', ?). So the author of
		// every commit a proxied-window handle will ever make is decided by the
		// env map PR2 projects — a map PR2 itself never writes through.
		//
		// The pin is therefore in two halves, and both are needed:
		//
		//   - cmd/gc's TestProxiedOpenEnvForPinGolden asserts the exact map
		//     production projects, including the author pair;
		//   - this row opens the library over a real proxy with that map's shape
		//     and asserts the Dolt commit that comes out carries `gc <gc@city>`.
		//
		// It opens the store directly rather than through a gc command because
		// there is no product path that writes through a proxied window in PR2 —
		// which is exactly why the pin cannot wait for one.
		//
		// And it opens the BARE storage through the proxied window, not
		// OpenNativeDoltStoreAtProxied (round3 review, completeness). Every
		// handle that function returns is read-only latched, structurally
		// (council B-F5), so a Create through it is refused before it reaches
		// the library and this row could only fail. OpenNativeStorageAtProxied
		// is the same window — the same BEADS_/BD_ withholding, the same
		// projection of this env map, the same library open that latches the
		// author — so the author the library commits with is decided exactly
		// as it is for the read handle. The write goes through gc's own
		// NativeDoltStore.Create over that storage, built with the exported
		// test constructor, because a writable proxied handle is PR3's to ask
		// for by name (WithProxiedWritable) and PR2 must not ship one.
		writeEnv := proxiedAuthorWindowEnv(t, cityRoot, city)
		storage, err := beads.OpenNativeStorageAtProxied(context.Background(), cityRoot, writeEnv)
		if err != nil {
			t.Fatalf("open the library over bd's proxy with the proxied window's env: %v", err)
		}
		store := beads.NewNativeDoltStoreOverStorageForTest(storage)
		defer store.CloseStore() //nolint:errcheck // test handle

		priority := 2
		created, err := store.Create(beads.Bead{
			Title:    "author pin " + strconv.FormatInt(time.Now().UnixNano(), 10),
			Type:     "task",
			Status:   "open",
			Priority: &priority,
		})
		if err != nil {
			t.Fatalf("write through the proxied window: %v", err)
		}
		t.Logf("wrote %s through a proxied-window handle", created.ID)

		db := proxiedScopeSQL(t, cityRoot)
		var committer, email string
		if err := db.QueryRowContext(context.Background(),
			"SELECT committer, email FROM dolt_log ORDER BY date DESC LIMIT 1").Scan(&committer, &email); err != nil {
			t.Fatalf("read the top Dolt commit's author: %v", err)
		}
		wantEmail := "gc@" + proxiedCityName(t, city)
		t.Logf("top Dolt commit author: %s <%s> (want gc <%s>)", committer, email, wantEmail)
		if committer != "gc" {
			t.Errorf("Dolt commit committer = %q, want %q: beads latches GIT_AUTHOR_NAME at open, so PR3's authorship is already decided by this projection",
				committer, "gc")
		}
		if email != wantEmail {
			t.Errorf("Dolt commit email = %q, want %q", email, wantEmail)
		}
		if strings.ContainsAny(email, "/\\ \t") {
			t.Errorf("the author email %q carries a path or whitespace; decision Q4 forbids a path-derived anchor, because a moved or reinstalled city would rewrite the same logical city's authorship",
				email)
		}
	})
}

// assertProxiedSchemaSkewRefusal requires the lane to have declined with a
// schema-skew verdict naming the given lane and direction.
//
// The store name and the verdict are asserted from the payload; the lane and
// direction are read from the message, because that is where doctor renders the
// observed and expected cursor pair and there is no structured field for them.
// That split is deliberate: the gate is the verdict, the message is the
// explanation, and a copy-edit of the explanation must not be able to turn a
// refusal into a pass.
func assertProxiedSchemaSkewRefusal(t *testing.T, payload beadsStorePayloadDoc, result doctorCheckResult, lane, direction string) {
	t.Helper()
	if payload.Store == "NativeDoltStore" {
		t.Fatalf("the lane served natively over a database whose %s lane is %s: %s\n  %s",
			lane, direction, describeProxiedAccount(payload), result.Message)
	}
	if payload.Proxied == nil || payload.Proxied.Verdict != "schema_skew" {
		t.Fatalf("a %s/%s cursor drift produced %s, want verdict schema_skew", lane, direction, describeProxiedAccount(payload))
	}
	if payload.PreflightGate != "proxied_provider" {
		t.Errorf("the healthy fallback changed its gate to %q; doctor's matcher and the topology matrix both key on proxied_provider", payload.PreflightGate)
	}
	if !strings.Contains(result.Message, "verdict=schema_skew") {
		t.Errorf("the beads-store message does not name the verdict: %s", result.Message)
	}
	// The status stays OK: the bd front door is a supported store for a proxied
	// scope, and a city that refuses to migrate itself is behaving correctly.
	if result.Status != "ok" {
		t.Errorf("beads-store = %s for a refused lane; declining to migrate somebody else's database is not a city fault: %s",
			result.Status, result.Message)
	}
}

// proxiedAuthorWindowEnv builds the environment the proxied open window projects,
// in the shape cmd/gc's nativeDoltProxiedOpenEnvForPin produces.
//
// It is assembled here rather than imported because that function lives in
// package main. The exact map production projects is pinned by
// TestProxiedOpenEnvForPinGolden in cmd/gc; what this row adds is the other end
// of the chain — that a map of this shape, over a real proxy, produces a Dolt
// commit authored `gc <gc@city>`.
func proxiedAuthorWindowEnv(t *testing.T, scopeRoot string, city *helpers.City) map[string]string {
	t.Helper()
	root := proxiedScopeProxyRoot(t, scopeRoot)
	var record struct {
		Port int `json:"port"`
	}
	readJSONFile(t, filepath.Join(root, "proxy.pid"), &record)
	var metadata proxiedBeadsMetadata
	readJSONFile(t, filepath.Join(scopeRoot, ".beads", "metadata.json"), &metadata)
	return map[string]string{
		"BEADS_DOLT_SERVER_MODE":     "1",
		"BEADS_DOLT_SERVER_HOST":     "127.0.0.1",
		"BEADS_DOLT_SERVER_PORT":     strconv.Itoa(record.Port),
		"BEADS_DOLT_SERVER_USER":     "root",
		"BEADS_DOLT_SERVER_DATABASE": strings.TrimSpace(metadata.DoltDatabase),
		"BEADS_DOLT_AUTO_START":      "0",
		"BEADS_DOLT_MAX_CONNS":       "1",
		"GIT_AUTHOR_NAME":            "gc",
		"GIT_AUTHOR_EMAIL":           "gc@" + proxiedCityName(t, city),
	}
}

// proxiedCityName is the configured workspace name, which is the author email's
// anchor (decision Q4). It is read from the city's own doctor output rather than
// guessed, so the row cannot pass by asserting the value it computed.
func proxiedCityName(t *testing.T, city *helpers.City) string {
	t.Helper()
	out, err := city.GC("doctor", "--json", "--check", "city-config")
	if err != nil {
		t.Logf("gc doctor --check city-config: %v\n%s", err, out)
	}
	var report doctorReport
	lastJSONLine(t, out, &report)
	for _, r := range report.Results {
		if r.Name != "city-config" {
			continue
		}
		// `effective city name "at-1234abcd"`
		if _, rest, ok := strings.Cut(r.Message, `effective city name "`); ok {
			if name, _, ok := strings.Cut(rest, `"`); ok && name != "" {
				return name
			}
		}
	}
	t.Fatalf("could not read the effective city name from doctor: %s", formatDoctorResults(report))
	return ""
}

func formatDoctorResults(report doctorReport) string {
	lines := make([]string, 0, len(report.Results))
	for _, r := range report.Results {
		lines = append(lines, fmt.Sprintf("  %s (%s): %s", r.Name, r.Status, r.Message))
	}
	return strings.Join(lines, "\n")
}
