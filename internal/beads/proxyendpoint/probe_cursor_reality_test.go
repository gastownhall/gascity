package proxyendpoint

import (
	"context"
	"strings"
	"testing"
)

// TestProbeSessionReadsTheIgnoredLanesCursorReality is council A-F2's pin.
//
// Before it, one probe session read exactly four statements — an existence
// probe and a MAX(version) for each cursor table — and reported the raw ignored
// number as if it were the number the linked library acts on. It is not.
// beads' migrationSource.currentVersion clamps a cursor the live schema
// contradicts down to a reality floor, and beads documents both contradicted
// shapes as ones that occur in the field. A database at raw ignored=26 with
// `leases.granted_node` absent is a database the library believes is at 11, so
// MigrateUp replays ignored 0012-0025 against it — and gc's proxied open is
// writable, because the library exports no read-only open to an embedder at
// v1.3.0.
//
// The session therefore reads the sentinels too, and the assertion is on the
// STATEMENTS as much as on the answer: a reader that concluded the right thing
// from evidence it never gathered would pass an outcome-only test.
func TestProbeSessionReadsTheIgnoredLanesCursorReality(t *testing.T) {
	const rawIgnored = 26

	cases := []struct {
		name          string
		absentTables  map[string]bool
		absentColumns map[string]bool
		ignored       int
		wantLimited   bool
		wantFloor     int
		wantMissing   string
		wantEffective int
	}{
		{
			name:          "a corroborated cursor is believed as read",
			ignored:       rawIgnored,
			wantEffective: rawIgnored,
		},
		{
			name:          "a missing sentinel column floors the lane at the replay floor",
			absentColumns: map[string]bool{"leases.granted_node": true},
			ignored:       rawIgnored,
			wantLimited:   true,
			wantFloor:     11,
			wantMissing:   "leases.granted_node",
			wantEffective: 11,
		},
		{
			name:          "a missing sentinel table floors the lane at zero",
			absentTables:  map[string]bool{"wisp_dependencies": true},
			ignored:       rawIgnored,
			wantLimited:   true,
			wantFloor:     0,
			wantMissing:   "wisp_dependencies",
			wantEffective: 0,
		},
		{
			// The library probes the tables first and short-circuits on them,
			// because they floor at zero and no column probe could lower that.
			name:          "a sentinel table outranks a sentinel column",
			absentTables:  map[string]bool{"wisps": true},
			absentColumns: map[string]bool{"leases.granted_node": true},
			ignored:       rawIgnored,
			wantLimited:   true,
			wantFloor:     0,
			wantMissing:   "wisps",
			wantEffective: 0,
		},
		{
			// A cursor of zero has nothing to contradict, so the library
			// returns before it consults the floor — and so does the probe,
			// which keeps the sentinel reads off the path of a database that
			// has never run the lane.
			name:          "an empty ignored lane is not probed for sentinels",
			absentTables:  map[string]bool{"wisps": true, "wisp_dependencies": true},
			absentColumns: map[string]bool{"leases.granted_node": true},
			ignored:       0,
			wantEffective: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeProbeConnector{
				cursors:       map[string]int64{mainCursorQuery: 66, ignoredCursorQuery: int64(tc.ignored)},
				absentTables:  tc.absentTables,
				absentColumns: tc.absentColumns,
			}
			report, err := readCursorsOver(context.Background(), fake)
			if err != nil {
				t.Fatalf("readCursorsOver: %v", err)
			}
			if report.Cursors.Ignored != tc.ignored {
				t.Fatalf("the probe reported ignored=%d, want the RAW on-disk %d: the payload an operator reads must be the disk truth",
					report.Cursors.Ignored, tc.ignored)
			}
			if report.Reality.Limited != tc.wantLimited {
				t.Fatalf("reality.Limited = %v, want %v (reality %+v)", report.Reality.Limited, tc.wantLimited, report.Reality)
			}
			if tc.wantLimited {
				if report.Reality.Floor != tc.wantFloor {
					t.Errorf("reality.Floor = %d, want %d", report.Reality.Floor, tc.wantFloor)
				}
				if report.Reality.Missing != tc.wantMissing {
					t.Errorf("reality.Missing = %q, want %q", report.Reality.Missing, tc.wantMissing)
				}
				if !strings.Contains(report.Reality.String(), tc.wantMissing) {
					t.Errorf("reality message %q does not name the missing sentinel", report.Reality.String())
				}
			}
			if got := report.Reality.EffectiveIgnored(report.Cursors.Ignored); got != tc.wantEffective {
				t.Fatalf("EffectiveIgnored(%d) = %d, want %d: this is the number migrationSource.atLatest computes, and therefore the number that decides whether MigrateUp replays the ignored series",
					report.Cursors.Ignored, got, tc.wantEffective)
			}

			// The evidence, not just the conclusion.
			probedColumn := false
			for _, statement := range fake.statements() {
				if strings.HasPrefix(statement, columnExistsQuery) {
					probedColumn = true
				}
			}
			wantColumnProbe := tc.ignored > 0 && len(tc.absentTables) == 0
			if probedColumn != wantColumnProbe {
				t.Fatalf("the session probed the sentinel column = %v, want %v; statements: %v",
					probedColumn, wantColumnProbe, fake.statements())
			}
		})
	}
}
