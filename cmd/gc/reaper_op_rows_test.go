package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/rollout"
)

// fakeReaperScopeBd answers the reaper guard's bd reads for one scope and
// records each call.
type fakeReaperScopeBd struct {
	census    string
	censusErr error
	help      string
	retained  string
	calls     []string
}

func (f *fakeReaperScopeBd) run(dir, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, dir+": "+name+" "+strings.Join(args, " "))
	switch {
	case len(args) > 0 && args[0] == "sql":
		if f.censusErr != nil {
			return nil, f.censusErr
		}
		return []byte("COUNT(*)\n" + f.census + "\n"), nil
	case len(args) == 2 && args[1] == "--help":
		return []byte("bd " + args[0] + " help\n" + f.help + "\n"), nil
	case len(args) == 4 && args[0] == "config" && args[1] == "get":
		return []byte(`{"key":"` + args[3] + `","value":"` + strings.ReplaceAll(f.retained, `"`, `\"`) + `"}`), nil
	}
	return nil, errors.New("unexpected bd call")
}

func (f *fakeReaperScopeBd) scope() reaperOpRowsScope {
	return reaperOpRowsScope{label: "city", dir: "/city", run: f.run}
}

const honorsRetainedTypesHelp = "Without --retain-ids it also skips beads whose type the workspace's types.retained setting names"

func TestReaperOpRowsHeldBack(t *testing.T) {
	for _, tc := range []struct {
		name    string
		bd      fakeReaperScopeBd
		want    string
		wantErr string
	}{
		{"no op rows", fakeReaperScopeBd{census: "0"}, "", ""},
		{"op rows, bd does not honor types.retained", fakeReaperScopeBd{census: "1", retained: "op"}, "holds op rows and its bd does not honor types.retained", ""},
		{"op rows, types.retained unset", fakeReaperScopeBd{census: "1", help: honorsRetainedTypesHelp}, "holds op rows and types.retained does not name op", ""},
		{"op rows, types.retained names other types", fakeReaperScopeBd{census: "1", help: honorsRetainedTypesHelp, retained: "operation,molecule"}, "holds op rows and types.retained does not name op", ""},
		{"op rows, types.retained names op", fakeReaperScopeBd{census: "1", help: honorsRetainedTypesHelp, retained: "molecule, op"}, "", ""},
		{"op rows, types.retained a JSON list naming op", fakeReaperScopeBd{census: "1", help: honorsRetainedTypesHelp, retained: `["op"]`}, "", ""},
		{"op rows, types.retained a mis-spelled list", fakeReaperScopeBd{census: "1", help: honorsRetainedTypesHelp, retained: "[op,fact]"}, "holds op rows and types.retained does not name op", ""},
		{"census fails", fakeReaperScopeBd{censusErr: errors.New("dolt down")}, "", "counting op rows: dolt down"},
		{"census unreadable", fakeReaperScopeBd{census: "id"}, "", `counting op rows: the census returned "COUNT(*)\nid"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bd := tc.bd
			got, err := reaperOpRowsHeldBack(bd.scope())
			gotErr := ""
			if err != nil {
				gotErr = err.Error()
			}
			if got != tc.want || gotErr != tc.wantErr {
				t.Fatalf("reaperOpRowsHeldBack = %q, %q; want %q, %q (calls %q)", got, gotErr, tc.want, tc.wantErr, bd.calls)
			}
		})
	}
}

// TestReaperOpRowsHeldBackProbesOnlyAScopeWithOpRows: a scope without op rows
// costs one bd read; one with them asks both verbs' help and the setting.
func TestReaperOpRowsHeldBackProbesOnlyAScopeWithOpRows(t *testing.T) {
	none := fakeReaperScopeBd{census: "0"}
	_, _ = reaperOpRowsHeldBack(none.scope())
	if len(none.calls) != 1 || !strings.Contains(none.calls[0], "bd sql --csv "+reaperOpRowCensusSQL) {
		t.Fatalf("calls = %q, want only the census", none.calls)
	}
	some := fakeReaperScopeBd{census: "1", help: honorsRetainedTypesHelp, retained: "op"}
	_, _ = reaperOpRowsHeldBack(some.scope())
	want := []string{
		"/city: bd sql --csv " + reaperOpRowCensusSQL,
		"/city: bd purge --help",
		"/city: bd prune --help",
		"/city: bd config get --json types.retained",
	}
	if strings.Join(some.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls = %q, want %q", some.calls, want)
	}
}

func TestOpsRecordingDoctorCheckWarnsOnScopesTheReaperHoldsBack(t *testing.T) {
	held := fakeReaperScopeBd{census: "3"}
	clean := fakeReaperScopeBd{census: "0"}
	retained := fakeReaperScopeBd{census: "3", help: honorsRetainedTypesHelp, retained: "op"}
	unreachable := fakeReaperScopeBd{censusErr: errors.New("bd: store unreachable")}
	c, _ := newTestOpsRecordingCheck(newOpSweepStore(), opSweepT0)
	c.reaperScopes = func() []reaperOpRowsScope {
		return []reaperOpRowsScope{
			{label: "city", dir: "/city", run: held.run},
			{label: "rig:api", dir: "/api", run: clean.run},
			{label: "rig:web", dir: "/web", run: retained.run},
			// Unreachable: the bead-store checks report it, not this one.
			{label: "rig:ops", dir: "/ops", run: unreachable.run},
		}
	}
	r := c.Run(&doctor.CheckContext{})
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want Warning: %q", r.Status, r.Message)
	}
	if !strings.HasPrefix(r.Message, "ops recording is off; ") || !strings.Contains(r.Message, "the core reaper does not purge or prune 1 scope(s) holding op rows") {
		t.Fatalf("message = %q", r.Message)
	}
	if len(r.Details) != 1 || r.Details[0] != "city: holds op rows and its bd does not honor types.retained" {
		t.Fatalf("details = %q", r.Details)
	}
	if !strings.Contains(r.FixHint, "gc bd config set types.retained op") {
		t.Fatalf("fix hint = %q", r.FixHint)
	}
}

func TestOpsRecordingDoctorCheckWithoutHeldScopesIsUnchanged(t *testing.T) {
	clean := fakeReaperScopeBd{census: "0"}
	c, _ := newTestOpsRecordingCheck(newOpSweepStore(), opSweepT0)
	c.reaperScopes = func() []reaperOpRowsScope {
		return []reaperOpRowsScope{{label: "city", dir: "/city", run: clean.run}}
	}
	if r := c.Run(&doctor.CheckContext{}); r.Status != doctor.StatusOK || r.Message != "ops recording is off" || len(r.Details) != 0 {
		t.Fatalf("result = %v %q %q, want OK, ops recording is off", r.Status, r.Message, r.Details)
	}
}

// TestOpsRecordingDoctorCheckKeepsAnErrorWhenTheReaperHoldsBack: a held
// scope adds its warning to an Error without lowering it.
func TestOpsRecordingDoctorCheckKeepsAnErrorWhenTheReaperHoldsBack(t *testing.T) {
	held := fakeReaperScopeBd{census: "1"}
	c, _ := newTestOpsRecordingCheck(nil, opSweepT0, rollout.WithOpsContent(rollout.Require))
	c.open = func() (beads.Store, error) { return nil, errors.New("store down") }
	c.reaperScopes = func() []reaperOpRowsScope {
		return []reaperOpRowsScope{{label: "city", dir: "/city", run: held.run}}
	}
	r := c.Run(&doctor.CheckContext{})
	if r.Status != doctor.StatusError || !strings.Contains(r.Message, "the core reaper does not purge or prune 1 scope(s)") {
		t.Fatalf("result = %v %q, want the Error kept with the reaper warning added", r.Status, r.Message)
	}
}

// reaperOpRowsScopesTestCity is a bd city (Dolt metadata) with rigs on every
// kind of scope the core reaper does or does not visit, and the path of rig
// cold, whose store the doctor store gate skips.
func reaperOpRowsScopesTestCity(t *testing.T) (string, *config.City, string) {
	t.Helper()
	t.Setenv("GC_BEADS", "")
	cityPath := orderBackendTestCity(t, `{"database":"dolt","backend":"dolt","dolt_database":"hq"}`)
	rig := func(name, metadata string) config.Rig {
		dir := filepath.Join(cityPath, "rigs", name)
		if metadata != "" {
			writeScopeBeadsMetadata(t, dir, metadata)
		}
		return config.Rig{Name: name, Path: dir}
	}
	dolt := func(db string) string {
		return `{"database":"dolt","backend":"dolt","dolt_database":"` + db + `"}`
	}
	api := rig("api", dolt("api"))
	cold := rig("cold", dolt("cold"))
	parked := rig("parked", dolt("parked"))
	parked.SuspendedOnStart = true
	filed := rig("filed", "")
	if err := os.MkdirAll(filepath.Join(filed.Path, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filed.Path, ".gc", "beads.json"), []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{
		api,
		cold,
		rig("served", `{"database":"beads","backend":"http"}`),
		parked,
		filed,
		{Name: "api-again", Path: api.Path},
	}}
	return cityPath, cfg, cold.Path
}

func reaperOpRowsScopeLabels(scopes []reaperOpRowsScope) []string {
	labels := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		labels = append(labels, scope.label)
	}
	return labels
}

// TestReaperOpRowsScopesAreTheReapersScopesDoctorMayRead: doctor probes the
// city and each rig the core reaper visits (a bd scope, not suspended, not
// served over http), each scope directory once, and never a scope the doctor
// store gate skips: on the proxied path any bd read of a stopped store starts
// its proxy and Dolt child, which doctor must never do.
func TestReaperOpRowsScopesAreTheReapersScopesDoctorMayRead(t *testing.T) {
	cityPath, cfg, cold := reaperOpRowsScopesTestCity(t)
	skipped := func(scopeRoot string) bool { return samePath(scopeRoot, cold) }
	if got, want := reaperOpRowsScopeLabels(reaperOpRowsScopes(cityPath, cfg, skipped)), []string{"city", "rig:api"}; !slices.Equal(got, want) {
		t.Fatalf("scopes = %q, want %q", got, want)
	}
	// The check doctor registers reads its scopes through the gate it is given.
	c := newOpsRecordingDoctorCheck(cityPath, cfg, rollout.ForTest(), nil, skipped)
	if c.reaperScopes == nil {
		t.Fatal("the ops-recording check probes no reaper scope")
	}
	if got, want := reaperOpRowsScopeLabels(c.reaperScopes()), []string{"city", "rig:api"}; !slices.Equal(got, want) {
		t.Fatalf("the check's scopes = %q, want %q", got, want)
	}
}

// reaperOpRowCensusFromScript is the op-row census reaper.sh takes, in
// database db.
func reaperOpRowCensusFromScript(t *testing.T, db string) string {
	t.Helper()
	reaper := readBundledPackFileForTest(t, "core", "assets/scripts/reaper.sh")
	m := regexp.MustCompile(`(?s)get_sql_count "op row" "(.*?)"; then`).FindStringSubmatch(reaper)
	if m == nil {
		t.Fatal(`reaper.sh no longer takes its op-row census with get_sql_count "op row"; update this test`)
	}
	census := strings.NewReplacer("\\`", "`", "$SCOPE_DB", db, "$OP_ROW_TYPE", reaperOpRowType).Replace(m[1])
	if !strings.Contains(census, "op_row_census") || strings.Contains(census, "$") {
		t.Fatalf("reaper.sh's op-row census reads %q", census)
	}
	return census
}

// TestReaperOpRowGuardProbesPassOnASplitCity: on a split city gc bd forwards
// only vetted commands, and a refused probe would hold the purge and the
// session prune back on every split city, op rows or none. The census as
// reaper.sh builds it, both help reads and the setting read pass the
// relocated-class read guard and every pinned bd inventory's judge.
func TestReaperOpRowGuardProbesPassOnASplitCity(t *testing.T) {
	probes := [][]string{
		{"sql", "--csv", reaperOpRowCensusFromScript(t, "hq")},
		{"purge", "--help"},
		{"prune", "--help"},
		{"config", "get", "--json", reaperRetainedTypesKey},
	}
	city := splitGuardTestCity()
	for _, argv := range probes {
		if msg, refused := bdSQLRelocatedClassRefusal(city, argv); refused {
			t.Errorf("the reaper's gc bd %s is refused as a relocated-class read: %s", argv[0], msg)
		}
	}
	for _, inv := range bdflags.Inventories() {
		deps := splitTestDeps(nil, func(string) (beads.Bead, bdReadResult) { return beads.Bead{}, bdReadFailed })
		pinned := inv
		deps.inventory = func() (*bdflags.Inventory, error) { return pinned, nil }
		for _, argv := range probes {
			if g := bdClassFlipGuard(city, t.TempDir(), argv, deps); g.refusal != "" {
				t.Errorf("%s: the reaper's gc bd %s %s is refused on a split city: %s", inv.Name, argv[0], argv[1], g.refusal)
			}
		}
	}
}

// TestReaperOpRowCensusSQLIsTheReapers: doctor's census is reaper.sh's, read
// in the scope's own database, so the two cannot disagree about which scopes
// hold op rows.
func TestReaperOpRowCensusSQLIsTheReapers(t *testing.T) {
	normalize := func(sql string) string {
		sql = regexp.MustCompile(`\s+`).ReplaceAllString(sql, " ")
		return regexp.MustCompile(` ?([()]) ?`).ReplaceAllString(strings.TrimSpace(sql), "$1")
	}
	reaper := strings.ReplaceAll(reaperOpRowCensusFromScript(t, "hq"), "`hq`.", "")
	if got, want := normalize(reaperOpRowCensusSQL), normalize(reaper); got != want {
		t.Fatalf("doctor's census\n  %s\nis not reaper.sh's\n  %s", got, want)
	}
}

// TestReaperRetainedTypesParseIsBds: doctor and reaper.sh read a stored
// types.retained value as bd's sweep does (ParseTypeList: a JSON array of
// strings, else a comma list, each name trimmed), so neither lets the purge
// run on a value from which bd keeps no op row. Each value goes through
// retainedTypesNameOp and through reaper.sh's own retained_types_name_op.
func TestReaperRetainedTypesParseIsBds(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is required to run reaper.sh's parser")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required to run reaper.sh's parser")
	}
	reaper := readBundledPackFileForTest(t, "core", "assets/scripts/reaper.sh")
	parser := regexp.MustCompile(`(?ms)^retained_types_name_op\(\) \{$.*?^\}$`).FindString(reaper)
	opRowType := regexp.MustCompile(`(?m)^OP_ROW_TYPE="([^"]*)"$`).FindStringSubmatch(reaper)
	if parser == "" || opRowType == nil {
		t.Fatal("reaper.sh no longer defines retained_types_name_op and OP_ROW_TYPE; update this test")
	}
	if opRowType[1] != reaperOpRowType {
		t.Fatalf("reaper.sh's OP_ROW_TYPE is %q, doctor's is %q", opRowType[1], reaperOpRowType)
	}
	for _, tc := range []struct {
		value   string
		namesOp bool
	}{
		{"op", true},
		{" op ", true},
		{"molecule, op", true},
		{"molecule,op,", true},
		{`["op"]`, true},
		{`["op","molecule"]`, true},
		{` ["molecule", " op "] `, true},
		{"[\n\"op\"\n]", true},
		{`["\u006fp"]`, true},
		// Go's JSON decoder leaves a null element empty, so bd reads ["op"].
		{`["op", null]`, true},
		// bd trims every Unicode space, a no-break space included.
		{"\u00a0op", true},
		{"", false},
		{"molecule", false},
		{"operation,molecule", false},
		{"OP", false},
		{"[op]", false},
		{"[op,fact]", false},
		{`"op"`, false},
		{"o p", false},
		{"op]", false},
		// Not JSON, so a comma list whose names are `["op"` and `]`.
		{`["op",]`, false},
		{`["op", 1]`, false},
		{`["op,fact"]`, false},
		{`[["op"]]`, false},
		{`["op"] ["x"]`, false},
		// A newline inside a name does not split it.
		{"x\nop", false},
		{"null", false},
		{"[]", false},
	} {
		out, err := json.Marshal(map[string]string{"key": reaperRetainedTypesKey, "value": tc.value})
		if err != nil {
			t.Fatal(err)
		}
		if got, err := retainedTypesNameOp(out); err != nil || got != tc.namesOp {
			t.Errorf("retainedTypesNameOp(%q) = %v, %v; want %v", tc.value, got, err, tc.namesOp)
		}
		cmd := exec.Command(bash, "-c", parser+"\nretained_types_name_op")
		cmd.Env = append(os.Environ(), "OP_ROW_TYPE="+reaperOpRowType)
		cmd.Stdin = bytes.NewReader(out)
		err = cmd.Run()
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			t.Fatalf("running reaper.sh's retained_types_name_op: %v", err)
		}
		if got := err == nil; got != tc.namesOp {
			t.Errorf("reaper.sh retained_types_name_op(%q) = %v, want %v", tc.value, got, tc.namesOp)
		}
	}
}

// TestBuildDoctorChecksOpsRecordingNeverProbesAStoppedRigStore: the
// ops-recording check walks the rigs from a city-level check, so the doctor
// store gate is what keeps its op-row probe from waking a stopped rig.
func TestBuildDoctorChecksOpsRecordingNeverProbesAStoppedRigStore(t *testing.T) {
	cityDir, cfg := doctorStoreGateCity(t)
	stubDoctorStoreLiveness(t, "rigstore")

	checks := buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{SkipCityDoltCheck: true, SkipManagedDoltCheck: true, SkipRigDoltChecks: true})
	for _, c := range checks {
		if ops, ok := c.(*opsRecordingDoctorCheck); ok {
			if got, want := reaperOpRowsScopeLabels(ops.reaperScopes()), []string{"city"}; !slices.Equal(got, want) {
				t.Fatalf("the ops-recording check probes %q, want %q: rig rigstore's store is stopped", got, want)
			}
			return
		}
	}
	t.Fatal("no ops-recording check registered")
}
