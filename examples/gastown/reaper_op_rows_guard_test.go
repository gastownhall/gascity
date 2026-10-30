package gastown_test

import (
	"strings"
	"testing"
)

// reaperOpRowCensusCase answers the reaper's op-row census with n rows.
func reaperOpRowCensusCase(n string) string {
	return `  *"op_row_census"*)
    printf 'COUNT(*)\n` + n + `\n'
    ;;
`
}

// newReaperOpRowsFixture is a city scope whose census finds opRows op rows,
// whose bd's purge and prune help say help, and whose database stores
// retained as types.retained. BD_PRUNE_COUNT is set so the session prune has
// candidates and reaches its gates.
func newReaperOpRowsFixture(t *testing.T, opRows, help, retained string) scopeFixture {
	t.Helper()
	f := newScopeFixture(t, reaperOpRowCensusCase(opRows))
	f.env["BD_PURGE_COUNT"] = "3"
	f.env["BD_PRUNE_COUNT"] = "2"
	f.env["BD_VERB_HELP_EXTRA"] = help
	f.env["BD_TYPES_RETAINED"] = retained
	return f
}

const reaperRetainedTypesHelp = "Without --retain-ids it also skips beads whose type the workspace's types.retained setting names"

func reaperPurged(bdLog string) bool {
	return strings.Contains(bdLog, "scope=city args=purge --wisps-plane --older-than 168h --json --force --limit 500")
}

func reaperPruned(bdLog string) bool {
	return strings.Contains(bdLog, "scope=city args=prune --pattern gm-* --older-than 720h --force --json")
}

// TestReaperWithoutOpRowsPurgesAndPrunesUnchanged: a scope with no op row is
// reaped as before, and the guard asks bd nothing beyond its census.
func TestReaperWithoutOpRowsPurgesAndPrunesUnchanged(t *testing.T) {
	f := newReaperOpRowsFixture(t, "0", "", "")
	out := f.runReaper(t)

	bdLog := f.read(t, f.bdLog)
	if !reaperPurged(bdLog) || !reaperPruned(bdLog) {
		t.Fatalf("a scope without op rows must be purged and pruned:\n%s", bdLog)
	}
	if strings.Contains(bdLog, "--help") || strings.Contains(bdLog, "args=config get") {
		t.Fatalf("with no op row the guard must not probe bd:\n%s", bdLog)
	}
	if !strings.Contains(f.read(t, f.doltLog), "op_row_census") {
		t.Fatalf("the reaper did not take its op-row census:\n%s", f.read(t, f.doltLog))
	}
	if outcome, ok := f.outcome(t); ok {
		t.Fatalf("no step was held back, but the run declared %+v\n%s", outcome, out)
	}
}

// TestReaperHoldsPurgeAndPruneBackOnAScopeWithOpRows covers every way a scope
// holding op rows is not yet safe to purge or prune: the purge and the prune
// are skipped there, the run says why, and nothing escalates.
func TestReaperHoldsPurgeAndPruneBackOnAScopeWithOpRows(t *testing.T) {
	for _, tc := range []struct {
		name, help, retained, why string
	}{
		{"bd does not honor types.retained", "", "op", "its bd does not honor types.retained"},
		{"types.retained unset", reaperRetainedTypesHelp, "", "types.retained does not name op"},
		{"types.retained names other types", reaperRetainedTypesHelp, "molecule,operation", "types.retained does not name op"},
		// bd reads these as the names `[op` and `fact]`, and `"op"`: no op.
		{"types.retained a list mis-spelled with brackets", reaperRetainedTypesHelp, "[op,fact]", "types.retained does not name op"},
		{"types.retained a name mis-spelled with quotes", reaperRetainedTypesHelp, `"op"`, "types.retained does not name op"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReaperOpRowsFixture(t, "4", tc.help, tc.retained)
			out := f.runReaper(t)

			bdLog := f.read(t, f.bdLog)
			if reaperPurged(bdLog) || reaperPruned(bdLog) {
				t.Fatalf("a scope with op rows that bd would delete untombstoned must be neither purged nor pruned:\n%s", bdLog)
			}
			if gcLog := f.read(t, f.gcLog); strings.Contains(gcLog, "ESCALATION") {
				t.Fatalf("a held-back purge is reported, not escalated:\n%s", gcLog)
			}
			outcome, ok := f.outcome(t)
			if !ok || outcome.Outcome != "partial" {
				t.Fatalf("outcome = %+v (declared %v), want partial", outcome, ok)
			}
			for _, step := range []string{"purge", "session prune"} {
				want := step + " skipped: holds op rows and " + tc.why
				if !outcome.hasScope("city", want) {
					t.Fatalf("outcome scopes = %+v, want city %q", outcome.Scopes, want)
				}
				if !strings.Contains(out, "reaper: city: "+want) {
					t.Fatalf("the run did not log %q:\n%s", want, out)
				}
			}
		})
	}
}

// TestReaperPurgesAndPrunesAScopeWithOpRowsItsBdRetains: once the scope's bd
// honors types.retained and its database names op, bd itself holds the op
// rows back, so the purge and the prune run.
func TestReaperPurgesAndPrunesAScopeWithOpRowsItsBdRetains(t *testing.T) {
	for _, retained := range []string{"op", "molecule, op", `["op","molecule"]`} {
		t.Run(retained, func(t *testing.T) {
			f := newReaperOpRowsFixture(t, "4", reaperRetainedTypesHelp, retained)
			f.runReaper(t)

			bdLog := f.read(t, f.bdLog)
			if !reaperPurged(bdLog) || !reaperPruned(bdLog) {
				t.Fatalf("types.retained=%s on a bd that honors it must leave the purge and prune running:\n%s", retained, bdLog)
			}
			for _, probe := range []string{"args=purge --help", "args=prune --help", "args=config get --json types.retained"} {
				if !strings.Contains(bdLog, "scope=city "+probe) {
					t.Fatalf("the guard did not ask bd %q:\n%s", probe, bdLog)
				}
			}
			if outcome, ok := f.outcome(t); ok {
				t.Fatalf("no step was held back, but the run declared %+v", outcome)
			}
		})
	}
}

// TestReaperHoldsPurgeBackWhenItsOpRowCensusFails: a census that cannot be
// read fails closed and is recorded as an anomaly.
func TestReaperHoldsPurgeBackWhenItsOpRowCensusFails(t *testing.T) {
	f := newReaperOpRowsFixture(t, "not-a-count", reaperRetainedTypesHelp, "op")
	f.runReaper(t)

	if bdLog := f.read(t, f.bdLog); reaperPurged(bdLog) || reaperPruned(bdLog) {
		t.Fatalf("an unreadable op-row census must hold the purge and prune back:\n%s", bdLog)
	}
	outcome, ok := f.outcome(t)
	if !ok || !outcome.hasScope("city", "purge skipped: op rows could not be counted") {
		t.Fatalf("outcome = %+v (declared %v), want the purge held back", outcome, ok)
	}
	if gcLog := f.read(t, f.gcLog); !strings.Contains(gcLog, "op row count returned non-numeric value") {
		t.Fatalf("the failed census was not recorded as an anomaly:\n%s", gcLog)
	}
}

// reaperOpRowCensusOn answers the op-row census of database db with n rows;
// the census of every other database falls through to the fixture's zero.
func reaperOpRowCensusOn(db, n string) string {
	return "  *'`" + db + "`.wisps WHERE issue_type'*op_row_census*)\n" +
		"    printf 'COUNT(*)\\n" + n + "\\n'\n" +
		"    ;;\n"
}

// TestReaperHoldsPurgeBackOnEachScopeHoldingOpRows: the guard is taken per bd
// scope, against that scope's own database. Only the scope whose census finds
// an op row is held back (one row in one table counts 1, the census's
// smallest positive answer); the scope beside it is purged as before, rig or
// city.
func TestReaperHoldsPurgeBackOnEachScopeHoldingOpRows(t *testing.T) {
	const why = "purge skipped: holds op rows and its bd does not honor types.retained"
	for _, tc := range []struct {
		name, opDB, held, purged string
	}{
		{"a rig holds op rows", "rigdb", "rig:api", "city"},
		{"the city holds op rows", "citydb", "city", "rig:api"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newScopeFixture(t, reaperOpRowCensusOn(tc.opDB, "1"))
			f.env["FAKE_RIG_LIST_JSON"] = `{"rigs":[{"name":"api","hq":false}]}`
			f.env["FAKE_SCOPE_DBS"] = "city=citydb rig:api=rigdb"
			f.env["BD_PURGE_COUNT"] = "3"
			f.runReaper(t)

			bdLog := f.read(t, f.bdLog)
			purge := " args=purge --wisps-plane --older-than 168h --json --force --limit 500"
			if !strings.Contains(bdLog, "scope="+tc.purged+purge) {
				t.Fatalf("%s holds no op row and must be purged:\n%s", tc.purged, bdLog)
			}
			if strings.Contains(bdLog, "scope="+tc.held+purge) {
				t.Fatalf("%s holds an op row and must not be purged:\n%s", tc.held, bdLog)
			}
			outcome, ok := f.outcome(t)
			if !ok || !outcome.hasScope(tc.held, why) || outcome.hasScope(tc.purged, why) {
				t.Fatalf("outcome = %+v (declared %v), want only %s %q", outcome, ok, tc.held, why)
			}
		})
	}
}
