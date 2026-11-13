package main

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// BenchmarkInfraMigrateMCShaped runs the whole city migration (plan, copy,
// verify, manifest, marker, clear) of an MC-shaped work store into the
// production SQLite binding, at the sizes S5 §3.5 names. The work store is a
// real SQLite store under the bead-policy wrapper, its payloads in the
// sidecar, so the engine's round trips on both sides are what is measured.
//
//	GOMAXPROCS=8 go test -run '^$' -bench BenchmarkInfraMigrateMCShaped -benchtime=1x ./cmd/gc/
func BenchmarkInfraMigrateMCShaped(b *testing.B) {
	for _, rows := range []int{1000, 10000, 40000} {
		b.Run(strconv.Itoa(rows), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				city := newMCShapedInfraCity(b, rows)
				source := city.sqliteSource(b)
				prev := openInfraMigrationSource
				openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
				cityPath := b.TempDir()
				cfg := infraSplitConfig(".gc/store")
				var log bytes.Buffer
				b.StartTimer()
				report := migrateInfraClasses(b, cityPath, cfg, &log)
				b.StopTimer()
				openInfraMigrationSource = prev
				if report.Outcome != infraMigrationConverged {
					b.Fatalf("outcome = %v: %s", report.Outcome, log.String())
				}
				b.ReportMetric(float64(city.infra), "rows")
				b.ReportMetric(float64(city.edges), "edges")
			}
		})
	}
}
