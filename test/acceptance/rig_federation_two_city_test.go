//go:build acceptance_a

// Two cities, one rig store: the federation = "rig" guard end to end.
//
// City A has rig alpha and city B has rig beta. Both rigs use the same bead
// prefix and both are bound to ONE database on one external Dolt server, the
// shape in which another city's rows are in every read and every claim this
// city makes. Both rigs set federation = "rig". Both cities import the same
// pack, so their agents' bare names, pool aliases and roster entries collide.
//
// The test walks the paths that reach that store from outside gc's own
// process as well as through it: `gc rig add` against the existing database,
// `gc ready` (through the controller's API and the in-process fallback),
// `gc bd` claims, raw bd claims, the core pack's orphan sweep as its order
// runs it, and the default on_death hook. Each city runs alone for a while
// (the other stopped) so every change to the shared rows is attributable to
// one city, and then both run together for several controller ticks. Every
// row another city owns, or no city may act on, must come out unchanged;
// each city's own dead claims must be released, so the test cannot pass by
// doing nothing.
package acceptance_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

const (
	// rf2SharedPrefix is the bead prefix of both rigs, and so the name of the
	// one database they share.
	rf2SharedPrefix = "sh"
	// rf2PatrolInterval is each city's controller tick.
	rf2PatrolInterval = 2 * time.Second
	// rf2Ticks is how many controller ticks (and orphan sweeps) each phase
	// runs before the shared rows are compared.
	rf2Ticks = 3
	// The 128-bit tokens of one dead pool session per city.
	rf2TokenA = "aaaaaaaa11111111aaaaaaaa11111111"
	rf2TokenB = "bbbbbbbb22222222bbbbbbbb22222222"
)

// The bd and dolt the test runs, resolved once.
var rf2BdPath, rf2DoltPath string

// rf2City is one of the two cities.
type rf2City struct {
	name     string // city directory name, also its workspace name
	hqPrefix string // the HQ bead prefix gc derives from the name
	rig      string
	city     *helpers.City
	rigDir   string
}

func (c *rf2City) poolSessionID(token string) string {
	return c.hqPrefix + "-session-" + token
}

// rf2Row is a seeded row of the shared store and the city that may act on it
// ("a", "b", or "" for no city).
type rf2Row struct {
	title    string
	status   string
	assignee string
	routedTo string
	owner    string
	id       string
}

// rf2State is one row as the shared store holds it.
type rf2State struct {
	ID       string            `json:"id"`
	Title    string            `json:"title"`
	Status   string            `json:"status"`
	Assignee string            `json:"assignee"`
	Metadata map[string]string `json:"metadata"`
}

func (s rf2State) key() string {
	return fmt.Sprintf("status=%s assignee=%q routed_to=%q", s.Status, s.Assignee, s.Metadata["gc.routed_to"])
}

func TestRigFederationTwoCitiesOneStore(t *testing.T) {
	started := time.Now()
	t.Cleanup(func() { t.Logf("two-city federation run took %s", time.Since(started).Round(time.Second)) })

	bdPath, doltPath := helpers.RequireTopologyTooling(t)
	rf2BdPath, rf2DoltPath = bdPath, doltPath
	root := helpers.TempDir(t)
	// No bd this test runs may start a Dolt of its own: on a port-bind
	// failure bd would silently auto-start a local server, and every row
	// would land somewhere other than the shared database.
	env := helpers.TopologyEnv(t, testEnv, root, bdPath, doltPath).Clone().
		With("BEADS_DOLT_AUTO_START", "0").
		With("GC_SESSION", "subprocess")

	up := helpers.StartExternalDolt(t, env, filepath.Join(root, "upstream"), "")
	projectIDs := map[string]string{}
	for _, db := range []struct{ name, prefix string }{
		{"hq_city_a", "ca"},
		{"hq_city_b", "cb"},
		{rf2SharedPrefix, rf2SharedPrefix},
	} {
		up.Database = db.name
		up.ProvisionBeadsDatabase(t, env, bdPath, filepath.Join(root, "provision-"+db.name), db.prefix)
		projectIDs[db.name] = up.ProjectID
	}

	pack := rf2WritePack(t, filepath.Join(root, "pack-pk"))
	a := &rf2City{name: "city-a", hqPrefix: "ca", rig: "alpha"}
	b := &rf2City{name: "city-b", hqPrefix: "cb", rig: "beta"}
	cities := []*rf2City{a, b}
	for _, c := range cities {
		c.city = helpers.NewCityAt(t, env, filepath.Join(root, c.name))
		c.rigDir = rf2GitRepo(t, filepath.Join(root, c.rig))
	}
	t.Cleanup(func() { rf2Teardown(t, env, root, up, cities) })

	for _, c := range cities {
		db := "hq_" + strings.ReplaceAll(c.name, "-", "_")
		rf2MustGC(t, env, "", "init", "--skip-provider-readiness", "--no-start", "--provider", "claude",
			"--dolt-host", up.Host, "--dolt-port", up.Port,
			"--dolt-database", db, "--dolt-project-id", projectIDs[db], c.city.Dir)
		rf2ImportPack(t, c.city.Dir, pack)
	}

	// First, gc's own rig add, in each city in turn, adopts the database that
	// already exists. City B's runs after city A's rig is bound to it.
	t.Run("rig-add-adopts-the-existing-database", func(t *testing.T) {
		for _, c := range cities {
			out, err := c.city.GC("rig", "add", c.rigDir, "--name", c.rig, "--prefix", rf2SharedPrefix, "--include", "pk="+pack)
			if err != nil {
				t.Fatalf("gc rig add %s in %s against the existing %q database: %v\n%s", c.rig, c.name, rf2SharedPrefix, err, out)
			}
			rf2GuardRig(t, c.city.Dir, c.rig)
		}
	})
	if t.Failed() {
		t.FailNow()
	}

	// probe is an unattributed open row: ready in both cities.
	var probe string
	t.Run("both-rigs-bind-the-one-upstream-database", func(t *testing.T) {
		for _, c := range cities {
			md := rf2Metadata(t, filepath.Join(c.rigDir, ".beads", "metadata.json"))
			if md["dolt_database"] != rf2SharedPrefix || md["project_id"] != projectIDs[rf2SharedPrefix] {
				t.Fatalf("rig %s metadata dolt_database=%q project_id=%q, want %q and the provisioned project id %q",
					c.rig, md["dolt_database"], md["project_id"], rf2SharedPrefix, projectIDs[rf2SharedPrefix])
			}
			out, err := c.city.GC("rig", "list", "--json")
			if err != nil || !strings.Contains(out, `"name":"`+c.rig+`"`) || !strings.Contains(out, `"federation":"rig"`) {
				t.Fatalf("gc rig list in %s does not show rig %s with federation = \"rig\": %v\n%s", c.name, c.rig, err, out)
			}
		}
		if info, err := os.Stat(filepath.Join(up.DataDir, rf2SharedPrefix)); err != nil || !info.IsDir() {
			t.Fatalf("the shared database is not in the upstream data dir %s: %v", up.DataDir, err)
		}
		probe = rf2Create(t, env, a.rigDir, rf2Row{title: "visibility probe"})
		if got := rf2Show(t, env, b.rigDir, probe); got.Title != "visibility probe" {
			t.Fatalf("a row created from rig alpha is not visible from rig beta: %+v", got)
		}
		if got := rf2UpstreamTitle(t, env, up, probe); got != "visibility probe" {
			t.Fatalf("the upstream server does not hold %s (got %q): the rows are not in the shared database", probe, got)
		}
		for _, c := range cities {
			if procs := helpers.DoltProcessesUnder(t, c.city.Dir); len(procs) != 0 {
				t.Fatalf("a Dolt server was started under %s:\n%s", c.city.Dir, strings.Join(procs, "\n"))
			}
		}
	})
	if t.Failed() {
		t.FailNow()
	}

	// Both cities start on one supervisor before the shared rows exist (a
	// start with them in place is the last step).
	for _, c := range cities {
		rf2MustGC(t, env, c.city.Dir, "start", c.city.Dir)
	}
	rf2WaitForLiveSession(t, env, cities)

	// Session records of each city's own: a dead pool session (pinned
	// 128-bit id) and a short-id session whose id both cities hold.
	for _, c := range cities {
		rf2SessionRecord(t, env, c.city.Dir, c.poolSessionID(c.tokenFor()), c.rig+"/pk.worker", "pk.worker-"+c.poolSessionID(c.tokenFor()))
		rf2SessionRecord(t, env, c.city.Dir, "ss-17", c.rig+"/pk.worker", "ss-17")
	}

	rows := rf2SeedRows(t, env, a, b)
	byTitle := map[string]*rf2Row{}
	for i := range rows {
		byTitle[rows[i].title] = &rows[i]
	}

	t.Run("ready-shows-each-city-only-its-own-rows", func(t *testing.T) {
		raw := rf2RawReady(t, env, a.rigDir)
		for _, title := range []string{"a-routed", "b-routed", "bare-routed", "case-twin", "unattributed"} {
			if !raw[byTitle[title].id] {
				t.Fatalf("precondition: raw bd ready does not list %s; the read below would pass vacuously", title)
			}
		}
		for _, c := range cities {
			own := "a-routed"
			if c == b {
				own = "b-routed"
			}
			want := []string{byTitle[own].id, byTitle["unattributed"].id, probe}
			sort.Strings(want)
			for _, mode := range []string{"api", "local"} {
				got := rf2GCReady(t, env, c, mode == "local")
				// Both controllers are already running, so a city may have
				// released one of its own seeded dead claims before this
				// read. The test seeds rows through raw bd, with no creation
				// stamp and no claim stamps, so a released row carries no
				// identity at all: it is Unattributed and ready in both
				// cities. Rows only move that way (nothing re-claims them
				// here), so a row Unattributed in a snapshot taken after the
				// read is allowed; every other row must be as seeded.
				after := rf2Snapshot(t, env, a.rigDir)
				allowed := slices.Clone(want)
				for _, r := range rows {
					if st := after[r.id]; st.Status == "open" && st.Assignee == "" && !rf2HasGCKeys(st.Metadata) {
						allowed = append(allowed, r.id)
					}
				}
				var missing, extra []string
				for _, id := range want {
					if !slices.Contains(got, id) {
						missing = append(missing, id)
					}
				}
				for _, id := range got {
					if !slices.Contains(allowed, id) {
						extra = append(extra, id)
					}
				}
				if len(missing) > 0 || len(extra) > 0 {
					// Name every unexpected row in full (type, creator,
					// metadata), so a row gc wrote itself can be traced.
					var shown []string
					for _, id := range extra {
						shown = append(shown, strings.TrimSpace(string(rf2Bd(t, env, a.rigDir, "show", id, "--json"))))
					}
					t.Errorf("gc ready (%s) in %s lists %v of the shared rig's ready rows, want %v (its own, the unattributed row and the visibility probe, plus any seeded row now Unattributed); missing %v; unexpected rows:\n%s", mode, c.name, got, want, missing, strings.Join(shown, "\n"))
				}
			}
		}
	})

	// Each city alone, the other city's rig suspended (its controller and
	// its orphan sweep leave a suspended rig alone): everything that changes
	// is that city's doing.
	before := rf2Snapshot(t, env, a.rigDir)
	for _, phase := range []struct {
		runs, idle *rf2City
	}{{b, a}, {a, b}} {
		c, other := phase.runs, phase.idle
		t.Run(c.name+"-alone-acts-only-on-its-own-rows", func(t *testing.T) {
			rf2MustGC(t, env, other.city.Dir, "rig", "suspend", other.rig)
			defer rf2MustGC(t, env, other.city.Dir, "rig", "resume", other.rig)
			// on_death first, so whatever this city's controller does next
			// with the rows it released (route recovery restores their
			// route) happens while it runs alone.
			rf2RunOnDeath(t, env, c.rigDir, &config.Agent{Name: "worker-1", BindingName: "pk", Dir: c.rig})
			rf2RunOnDeath(t, env, c.city.Dir, &config.Agent{Name: "intake-2", BindingName: "pk"})
			var sweeps []string
			for i := 0; i < rf2Ticks; i++ {
				out, err := c.city.GC("order", "run", "orphan-sweep")
				if err != nil {
					t.Fatalf("gc order run orphan-sweep in %s: %v\n%s", c.name, err, out)
				}
				sweeps = append(sweeps, out)
				time.Sleep(rf2PatrolInterval)
			}

			after := rf2Snapshot(t, env, a.rigDir)
			released := map[string]bool{
				c.rig + "-dead-pool-name": true, // own pool session's claim, by the orphan sweep
				c.rig + "-qualified":      true, // own rig agent's claim, by on_death
			}
			for _, r := range rows {
				was, now := before[r.id], after[r.id]
				if released[r.title] {
					if now.Status != "open" || now.Assignee != "" {
						t.Errorf("%s did not release its own dead claim %s (%s): %s\nsweeps:\n%s", c.name, r.id, r.title, now.key(), strings.Join(sweeps, "\n"))
					}
					if !c.ownsValue(now.Metadata["gc.routed_to"]) {
						t.Errorf("%s routed its released row %s (%s) outside its rig: %s", c.name, r.id, r.title, now.key())
					}
					continue
				}
				if r.owner == c.short() && r.title == c.rig+"-dead-pool-id" {
					continue // checked below
				}
				if was.key() != now.key() {
					t.Errorf("%s changed %s (%s, owner %q): %s -> %s\nsweeps:\n%s", c.name, r.id, r.title, r.owner, was.key(), now.key(), strings.Join(sweeps, "\n"))
				}
			}
			for _, out := range sweeps {
				if !strings.Contains(out, "claims outside guarded rig namespaces ("+c.rig+": ") {
					t.Errorf("an orphan sweep in %s counted no claim outside its rig's namespace:\n%s", c.name, out)
				}
				if !strings.Contains(out, "its bead id or assignee contains a tab or line break") {
					t.Errorf("an orphan sweep in %s did not protect the forged row:\n%s", c.name, out)
				}
			}
			before = after
		})
	}

	// A claim held under a pool session's bead id (how an unaliased pool
	// worker claims since sessions run under their bead id) is this city's
	// own, and its session is dead: the orphan sweep must release it.
	t.Run("orphan-sweep-releases-an-own-dead-pool-session-id-claim", func(t *testing.T) {
		now := rf2Snapshot(t, env, a.rigDir)
		for _, c := range cities {
			r := byTitle[c.rig+"-dead-pool-id"]
			if got := now[r.id]; got.Status != "open" || got.Assignee != "" {
				t.Errorf("%s's orphan sweep did not release its own dead pool session's claim %s: %s (gc bd release-if-current, given no --rig, routes by the assignee %q, an HQ-prefixed session bead id, and answers from the HQ store)",
					c.name, r.id, got.key(), r.assignee)
			}
		}
	})

	// Both cities running together for several ticks: no row neither city
	// owns moves, and an owned row's identities stay its owner's.
	t.Run("both-cities-together-have-no-cross-city-effect", func(t *testing.T) {
		start := rf2Snapshot(t, env, a.rigDir)
		for i := 0; i < rf2Ticks; i++ {
			for _, c := range cities {
				if out, err := c.city.GC("order", "run", "orphan-sweep"); err != nil {
					t.Fatalf("gc order run orphan-sweep in %s: %v\n%s", c.name, err, out)
				}
			}
			time.Sleep(rf2PatrolInterval)
		}
		end := rf2Snapshot(t, env, a.rigDir)
		for _, r := range rows {
			was, now := start[r.id], end[r.id]
			if was.key() == now.key() {
				continue
			}
			owner := map[string]*rf2City{"a": a, "b": b}[r.owner]
			if owner == nil || was.Status != now.Status || was.Assignee != now.Assignee || !owner.ownsValue(now.Metadata["gc.routed_to"]) {
				t.Errorf("%s (%s, owner %q) changed with both cities running: %s -> %s", r.id, r.title, r.owner, was.key(), now.key())
			}
		}
	})

	t.Run("claims-through-gc-bd-and-raw-bd", func(t *testing.T) {
		actor := b.poolSessionID(rf2TokenB)
		for _, title := range []string{"a-routed", "bare-routed", "case-twin"} {
			r := byTitle[title]
			out, err := rf2GCWithActor(env, b.city.Dir, actor, "bd", "--rig", "beta", "update", r.id, "--claim")
			if err == nil || !strings.Contains(out, `rig "beta" (federation = "rig") does not own it`) {
				t.Errorf("city B claimed %s (%s) through gc bd: %v\n%s", r.id, title, err, out)
			}
		}
		r := byTitle["b-routed"]
		out, err := rf2GCWithActor(env, b.city.Dir, "pk.intake-2", "bd", "--rig", "beta", "update", r.id, "--claim")
		if err == nil || !strings.Contains(out, `actor "pk.intake-2" is outside it`) {
			t.Errorf("city B claimed %s under the same-pack alias pk.intake-2: %v\n%s", r.id, err, out)
		}
		if out, err := rf2GCWithActor(env, b.city.Dir, actor, "bd", "--rig", "beta", "update", r.id, "--claim"); err != nil {
			t.Errorf("city B could not claim its own row %s as its pool session: %v\n%s", r.id, err, out)
		}
		// Raw bd is outside the guard; bd's own claim fencing still refuses
		// a row another city holds.
		held := byTitle["a-live"]
		cmd := exec.Command(bdPath, "update", held.id, "--claim") //nolint:gosec // the configured test bd
		cmd.Dir = b.rigDir
		cmd.Env = env.Clone().With("BEADS_ACTOR", "beta/pk.worker-1").ToolList()
		if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "already claimed") {
			t.Errorf("raw bd claimed %s, held by city A, from rig beta: %v\n%s", held.id, err, out)
		}
		final := rf2Snapshot(t, env, a.rigDir)
		for _, title := range []string{"a-routed", "bare-routed", "case-twin", "a-live"} {
			rr := byTitle[title]
			if final[rr.id].key() != before[rr.id].key() {
				t.Errorf("%s (%s) changed after refused claims: %s -> %s", rr.id, title, before[rr.id].key(), final[rr.id].key())
			}
		}
		if got := final[r.id]; got.Status != "in_progress" || got.Assignee != actor {
			t.Errorf("city B's own claim of %s did not land: %s", r.id, got.key())
		}
	})

	// A guarded city restarts while the shared store holds work its own pool
	// sessions claimed, the steady state of any city with a rig pool. The
	// newest row is such a claim, so a bounded discovery read of the rig's
	// store meets it first.
	t.Run("restart-with-own-pool-claims-in-the-shared-store", func(t *testing.T) {
		rf2Create(t, env, a.rigDir, rf2Row{title: "a-live-pool", status: "in_progress", assignee: a.poolSessionID(rf2TokenA)})
		rf2MustGC(t, env, a.city.Dir, "stop", a.city.Dir)
		if out, err := a.city.GC("start", a.city.Dir); err != nil {
			t.Fatalf("city A does not start again with its own pool session's claim in the shared store: %v\n%s", err, out)
		}
		rf2WaitForLiveSession(t, env, []*rf2City{a})
	})
}

func (c *rf2City) tokenFor() string {
	if c.rig == "alpha" {
		return rf2TokenA
	}
	return rf2TokenB
}

// ownsValue reports whether an owner-key value is empty or inside the
// city's rig namespace.
func (c *rf2City) ownsValue(v string) bool {
	return v == "" || strings.HasPrefix(v, c.rig+"/")
}

func (c *rf2City) short() string {
	if c.rig == "alpha" {
		return "a"
	}
	return "b"
}

// rf2SeedRows writes the shared rows through raw bd from rig alpha (a writer
// outside gc, so nothing stamps them). The owner is the city whose guard may
// act on the row; "" means neither.
func rf2SeedRows(t *testing.T, env *helpers.Env, a, b *rf2City) []rf2Row {
	t.Helper()
	rows := []rf2Row{
		{title: "a-routed", status: "open", routedTo: "alpha/pk.worker", owner: "a"},
		{title: "b-routed", status: "open", routedTo: "beta/pk.worker", owner: "b"},
		{title: "bare-routed", status: "open", routedTo: "pk.worker"},
		{title: "case-twin", status: "open", routedTo: "Beta/pk.worker"},
		{title: "unattributed", status: "open"},
		{title: "a-live", status: "in_progress", assignee: "alpha/pk.worker-2", owner: "a"},
		{title: "alpha-qualified", status: "in_progress", assignee: "alpha/pk.worker-1", owner: "a"},
		{title: "beta-qualified", status: "in_progress", assignee: "beta/pk.worker-1", owner: "b"},
		{title: "bare-instance", status: "in_progress", assignee: "pk.worker-1"},
		{title: "alpha-dead-pool-name", status: "in_progress", assignee: "pk.worker-" + a.poolSessionID(rf2TokenA), owner: "a"},
		{title: "beta-dead-pool-name", status: "in_progress", assignee: "pk.worker-" + b.poolSessionID(rf2TokenB), owner: "b"},
		{title: "alpha-dead-pool-id", status: "in_progress", assignee: a.poolSessionID(rf2TokenA), owner: "a"},
		{title: "beta-dead-pool-id", status: "in_progress", assignee: b.poolSessionID(rf2TokenB), owner: "b"},
		{title: "alias-claim", status: "in_progress", assignee: "pk.intake-2"},
		{title: "short-claim", status: "in_progress", assignee: "ss-17"},
		{title: "person-claim", status: "in_progress", assignee: "alice"},
		{title: "person-on-beta", status: "in_progress", assignee: "alice", routedTo: "beta/pk.worker"},
		{title: "forged", status: "in_progress", assignee: b.poolSessionID(rf2TokenB) + "\trig:beta\tx"},
	}
	for i := range rows {
		rows[i].id = rf2Create(t, env, a.rigDir, rows[i])
	}
	return rows
}

// rf2WritePack writes the pack both cities import: a rig-scoped pool, a
// city-scoped pool, and a city-scoped keeper session that keeps one session
// live (the orphan sweep skips a city with none). The pools never start a
// session.
func rf2WritePack(t *testing.T, dir string) string {
	t.Helper()
	// Every agent runs rf2-agent, whose command line names the pack
	// directory (under the test root), so teardown can find any session
	// process gc failed to stop and kill it by PID (rf2KillLeftovers).
	agent := filepath.Join(dir, "bin", "rf2-agent")
	rf2WriteFile(t, agent, "#!/bin/sh\ntrap 'kill $child 2>/dev/null; exit 0' TERM INT HUP\nsleep 3600 &\nchild=$!\nwait $child\n")
	if err := os.Chmod(agent, 0o755); err != nil {
		t.Fatal(err)
	}
	start := fmt.Sprintf("start_command = %q\n", "/bin/sh "+agent)
	files := map[string]string{
		"pack.toml":                "[pack]\nname = \"pk\"\nschema = 2\n\n[[named_session]]\ntemplate = \"keeper\"\nscope = \"city\"\nmode = \"always\"\n",
		"agents/worker/agent.toml": "scope = \"rig\"\nmax_active_sessions = 0\n" + start,
		"agents/worker/prompt.md":  "You work.\n",
		"agents/intake/agent.toml": "scope = \"city\"\nmax_active_sessions = 0\n" + start,
		"agents/intake/prompt.md":  "You take intake.\n",
		"agents/keeper/agent.toml": "scope = \"city\"\n" + start,
		"agents/keeper/prompt.md":  "You keep.\n",
	}
	for rel, body := range files {
		rf2WriteFile(t, filepath.Join(dir, rel), body)
	}
	return dir
}

// rf2ImportPack makes the city import the shared pack and nothing it would
// fetch: it drops the default role pack import, its named session, and the
// default rig import, and sets a short controller tick.
func rf2ImportPack(t *testing.T, cityDir, pack string) {
	t.Helper()
	packPath := filepath.Join(cityDir, "pack.toml")
	p := rf2ReadFile(t, packPath)
	p = rf2DropTable(p, "[imports.gc]")
	p = rf2DropTable(p, "[[named_session]]")
	if !strings.Contains(p, "[imports]\n") {
		t.Fatalf("pack.toml has no [imports] table:\n%s", p)
	}
	p = strings.Replace(p, "[imports]\n", "[imports]\n[imports.pk]\nsource = "+fmt.Sprintf("%q", pack)+"\n", 1)
	rf2WriteFile(t, packPath, p)

	cityPath := filepath.Join(cityDir, "city.toml")
	c := rf2ReadFile(t, cityPath)
	c = rf2DropTable(c, "[defaults.rig.imports.gc]")
	c += fmt.Sprintf("\n[daemon]\npatrol_interval = %q\nstart_ready_timeout = \"10m\"\n", rf2PatrolInterval.String())
	rf2WriteFile(t, cityPath, c)
}

// rf2GuardRig sets federation = "rig" and namespace = "exclusive" on the rig
// gc rig add just appended.
func rf2GuardRig(t *testing.T, cityDir, rig string) {
	t.Helper()
	path := filepath.Join(cityDir, "city.toml")
	c := rf2ReadFile(t, path)
	head := "[[rigs]]\nname = " + fmt.Sprintf("%q", rig) + "\n"
	if !strings.Contains(c, head) {
		t.Fatalf("city.toml has no [[rigs]] entry for %s:\n%s", rig, c)
	}
	c = strings.Replace(c, head, head+"federation = \"rig\"\nnamespace = \"exclusive\"\n", 1)
	rf2WriteFile(t, path, c)
}

// rf2DropTable removes a TOML table header and its keys (up to the next
// header) from text.
func rf2DropTable(text, header string) string {
	lines := strings.Split(text, "\n")
	var out []string
	skipping := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			skipping = trimmed == header
		}
		if !skipping {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func rf2GitRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", dir},
		{"-C", dir, "config", "user.email", "test@test.com"},
		{"-C", dir, "config", "user.name", "Test"},
		{"-C", dir, "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil { //nolint:gosec // fixed argv
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// rf2Bd runs the configured bd in dir. BEADS_DOLT_AUTO_START=0 comes from env.
func rf2Bd(t *testing.T, env *helpers.Env, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(rf2BdPath, args...) //nolint:gosec // the configured test bd
	cmd.Dir = dir
	cmd.Env = env.ToolList()
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("bd %v in %s: %v\n%s%s", args, dir, err, out, stderr)
	}
	return out
}

func rf2Create(t *testing.T, env *helpers.Env, dir string, r rf2Row) string {
	t.Helper()
	md := "{}"
	if r.routedTo != "" {
		md = fmt.Sprintf(`{"gc.routed_to":%q}`, r.routedTo)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rf2Bd(t, env, dir, "create", r.title, "--type", "task", "--metadata", md, "--json"), &created); err != nil || created.ID == "" {
		t.Fatalf("bd create %q: %v", r.title, err)
	}
	if r.assignee != "" {
		rf2Bd(t, env, dir, "update", created.ID, "--assignee", r.assignee, "--status", r.status)
	}
	return created.ID
}

func rf2Show(t *testing.T, env *helpers.Env, dir, id string) rf2State {
	t.Helper()
	var rows []rf2State
	if err := json.Unmarshal(rf2Bd(t, env, dir, "show", id, "--json"), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("bd show %s: %v", id, err)
	}
	return rows[0]
}

// rf2HasGCKeys reports whether a row carries any gc.* metadata (an owner
// key, a claim stamp or the creation stamp). The orphan sweep's release note,
// gc.orphan_sweep_reason, is not one: it names no city, and a dead claim the
// sweep released carries it and is Unattributed.
func rf2HasGCKeys(md map[string]string) bool {
	for k := range md {
		if strings.HasPrefix(k, "gc.") && k != "gc.orphan_sweep_reason" {
			return true
		}
	}
	return false
}

// rf2Snapshot reads every row of the shared store.
func rf2Snapshot(t *testing.T, env *helpers.Env, dir string) map[string]rf2State {
	t.Helper()
	var rows []rf2State
	if err := json.Unmarshal(rf2Bd(t, env, dir, "list", "--all", "--limit=0", "--json"), &rows); err != nil {
		t.Fatalf("bd list: %v", err)
	}
	out := make(map[string]rf2State, len(rows))
	for _, r := range rows {
		out[r.ID] = r
	}
	return out
}

func rf2RawReady(t *testing.T, env *helpers.Env, dir string) map[string]bool {
	t.Helper()
	var rows []rf2State
	if err := json.Unmarshal(rf2Bd(t, env, dir, "ready", "--limit=0", "--json"), &rows); err != nil {
		t.Fatalf("bd ready: %v", err)
	}
	out := map[string]bool{}
	for _, r := range rows {
		out[r.ID] = true
	}
	return out
}

// rf2GCReady returns the ids of the shared rig's rows gc ready lists in c,
// through the controller's API or (local) the in-process read.
func rf2GCReady(t *testing.T, env *helpers.Env, c *rf2City, local bool) []string {
	t.Helper()
	e := env.Clone()
	if local {
		e = e.With("GC_NO_API", "1")
	}
	out, _, err := helpers.RunGCStreams(e, c.city.Dir, "ready")
	if err != nil {
		t.Fatalf("gc ready in %s: %v\n%s", c.name, err, out)
	}
	var rows []rf2State
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("gc ready in %s: %v\n%s", c.name, err, out)
	}
	var ids []string
	for _, r := range rows {
		if strings.HasPrefix(r.ID, rf2SharedPrefix+"-") {
			ids = append(ids, r.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// rf2SessionRecord writes a session record into the city's own (HQ) store.
// The session is dead: the record is closed.
func rf2SessionRecord(t *testing.T, env *helpers.Env, cityDir, id, template, sessionName string) {
	t.Helper()
	md := fmt.Sprintf(`{"template":%q,"session_name":%q}`, template, sessionName)
	rf2Bd(t, env, cityDir, "create", "session record", "--id", id, "--force", "--type", "session", "--labels", "gc:session", "--metadata", md, "--json")
	rf2Bd(t, env, cityDir, "close", id)
}

// rf2RunOnDeath runs the agent's default on_death hook in dir, the way the
// controller runs it when one of its sessions dies. It runs as the dead
// agent (BEADS_ACTOR): bd refuses to clear a claim for any other actor
// unless forced, and what this test asks of the hook is which rows it
// selects, not whose name bd records for the write.
func rf2RunOnDeath(t *testing.T, env *helpers.Env, dir string, agent *config.Agent) {
	t.Helper()
	hook := agent.EffectiveOnDeath()
	cmd := exec.Command("sh", "-c", hook)
	cmd.Dir = dir
	cmd.Env = env.Clone().With("BEADS_ACTOR", agent.QualifiedName()).ToolList()
	out, err := cmd.CombinedOutput()
	if err != nil || strings.Contains(string(out), "gc-recovery:") {
		t.Fatalf("on_death of %s in %s: %v\n%s", agent.QualifiedName(), dir, err, out)
	}
}

// rf2UpstreamTitle reads a row's title straight from the upstream server.
func rf2UpstreamTitle(t *testing.T, env *helpers.Env, up *helpers.ExternalDolt, id string) string {
	t.Helper()
	cmd := exec.Command(rf2DoltPath, "--host", up.Host, "--port", up.Port, "--user", "root", "--password", "", "--no-tls",
		"--use-db", rf2SharedPrefix, "sql", "-r", "csv", "-q", fmt.Sprintf("select title from issues where id = '%s'", id))
	cmd.Dir = up.DataDir
	cmd.Env = env.ToolList()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dolt sql against the upstream: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func rf2Metadata(t *testing.T, path string) map[string]string {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal([]byte(rf2ReadFile(t, path)), &raw); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string]string{}
	for k, v := range raw {
		out[k] = fmt.Sprint(v)
	}
	return out
}

// rf2WaitForLiveSession waits until each city's keeper session is active.
func rf2WaitForLiveSession(t *testing.T, env *helpers.Env, cities []*rf2City) {
	t.Helper()
	for _, c := range cities {
		deadline := time.Now().Add(2 * time.Minute)
		for {
			out, _, err := helpers.RunGCStreams(env, c.city.Dir, "session", "list", "--json")
			if err == nil && strings.Contains(out, `"pk.keeper"`) && strings.Contains(out, `"active"`) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("no live session in %s: %v\n%s", c.name, err, out)
			}
			time.Sleep(time.Second)
		}
	}
}

func rf2GCWithActor(env *helpers.Env, dir, actor string, args ...string) (string, error) {
	return helpers.RunGC(env.Clone().With("BEADS_ACTOR", actor), dir, args...)
}

func rf2MustGC(t *testing.T, env *helpers.Env, dir string, args ...string) string {
	t.Helper()
	out, err := helpers.RunGC(env, dir, args...)
	if err != nil {
		t.Fatalf("gc %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// rf2Teardown stops both cities and the supervisor, retires the upstream,
// and fails if any Dolt process under root survived.
func rf2Teardown(t *testing.T, env *helpers.Env, root string, up *helpers.ExternalDolt, cities []*rf2City) {
	for _, c := range cities {
		helpers.RunGC(env, c.city.Dir, "stop", c.city.Dir) //nolint:errcheck // best effort
	}
	helpers.RunGC(env, "", "supervisor", "stop", "--wait") //nolint:errcheck // best effort
	for _, c := range cities {
		helpers.RunGC(env, c.city.Dir, "unregister", c.city.Dir) //nolint:errcheck // best effort
	}
	up.Stop()
	if leaked := helpers.WaitForNoDoltProcesses(t, root, 20*time.Second); len(leaked) != 0 {
		t.Errorf("Dolt processes survived under %s:\n%s", root, strings.Join(leaked, "\n"))
	}
	rf2KillLeftovers(t, root)
}

// rf2KillLeftovers kills, by exact PID, every process still running whose
// command line names root (agent sessions run rf2-agent from under it), and
// its process group when it leads one (the subprocess runtime starts each
// session in a group of its own, so the agent's sleep child goes with it).
// Nothing outside root is touched. A leftover is logged: gc stop should have
// stopped it.
func rf2KillLeftovers(t *testing.T, root string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, err := exec.Command("ps", "-eo", "pid=,pgid=,args=").Output()
		if err != nil {
			t.Errorf("read process table: %v", err)
			return
		}
		self := os.Getpid()
		var left []string
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 3 || !strings.Contains(line, root) {
				continue
			}
			pid, perr := strconv.Atoi(fields[0])
			pgid, gerr := strconv.Atoi(fields[1])
			if perr != nil || gerr != nil || pid == self || pid <= 1 {
				continue
			}
			left = append(left, strings.TrimSpace(line))
			if pgid == pid {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		if len(left) == 0 {
			return
		}
		t.Logf("killed processes gc left running under %s:\n%s", root, strings.Join(left, "\n"))
		if time.Now().After(deadline) {
			t.Errorf("processes under %s survived SIGKILL:\n%s", root, strings.Join(left, "\n"))
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func rf2ReadFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func rf2WriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
