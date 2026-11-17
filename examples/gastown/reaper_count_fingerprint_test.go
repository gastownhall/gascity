//go:build unix

package gastown_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// countFingerprintCases answers the reaper's stale-wisp and mail-wisp counts
// from the environment, per scope: the api rig's database is apidb, every
// other scope is the city's. The api arm comes first because the stale-wisp
// query of every scope has the same shape.
const countFingerprintCases = `
  *"apidb"*"wisps"*"issue_type NOT IN ('message')"*"created_at <"*)
    printf 'COUNT(*)\n%s\n' "${API_STALE_WISPS:-0}"
    ;;
  *"wisps"*"issue_type NOT IN ('message')"*"created_at <"*)
    printf 'COUNT(*)\n%s\n' "${CITY_STALE_WISPS:-0}"
    ;;
  *"wisps"*"issue_type = 'message'"*)
    printf 'COUNT(*)\n%s\n' "${CITY_MAIL_WISPS:-0}"
    ;;`

// countFingerprintFixture is a city with an api rig whose incidents are
// deduplicated by escalate.sh (a 24h reminder, so only a changed fingerprint
// notifies inside the test). A session prune candidate exists, so a stale
// backup is itself an anomaly.
func countFingerprintFixture(t *testing.T) scopeFixture {
	t.Helper()
	f := sessionPruneFixture(t, countFingerprintCases)
	f.env["FAKE_RIG_LIST_JSON"] = `{"rigs":[{"name":"api","hq":false}]}`
	f.env["FAKE_SCOPE_DBS"] = "rig:api=apidb"
	f.env["GC_ESCALATE_INCIDENT_DIR"] = filepath.Join(f.cityDir, ".gc", "incidents")
	f.env["GC_ESCALATE_REMINDER_SECONDS"] = "86400"
	return f
}

type countFingerprintStep struct {
	name string
	env  map[string]string
	// want is the cumulative number of anomaly escalations after this run.
	want int
}

func runCountFingerprintSteps(t *testing.T, f scopeFixture, steps []countFingerprintStep) {
	t.Helper()
	const notified = "ESCALATION: Reaper anomalies detected"
	for _, s := range steps {
		for _, k := range []string{"CITY_STALE_WISPS", "API_STALE_WISPS", "CITY_MAIL_WISPS"} {
			f.env[k] = "0"
		}
		for k, v := range s.env {
			f.env[k] = v
		}
		f.runReaper(t)
		if got := strings.Count(f.read(t, f.gcLog), notified); got != s.want {
			t.Fatalf("after %q: %d anomaly escalations, want %d:\n%s", s.name, got, s.want, f.read(t, f.gcLog))
		}
	}
}

// TestReaperStaleWispCountDriftStaysOneIncident pins the incident fingerprint
// of a standing stale-wisp backlog (ga-39arh8e): its count drifts on almost
// every 30-minute run, so a fingerprint that embeds the exact count mailed
// the operator every run. The count is fingerprinted by its order of
// magnitude: a drift inside one decade stays one incident, while a tenfold
// jump, a second scope and a return after a clean run each notify.
func TestReaperStaleWispCountDriftStaysOneIncident(t *testing.T) {
	f := countFingerprintFixture(t)
	f.env["BD_BACKUP_STATUS_JSON"] = freshBackupStatusJSON(time.Hour, false)
	runCountFingerprintSteps(t, f, []countFingerprintStep{
		{"2047 stale wisps in the city", map[string]string{"CITY_STALE_WISPS": "2047"}, 1},
		{"the same backlog at 2049", map[string]string{"CITY_STALE_WISPS": "2049"}, 1},
		{"the same backlog at 3500", map[string]string{"CITY_STALE_WISPS": "3500"}, 1},
		{"a tenfold jump to 12047", map[string]string{"CITY_STALE_WISPS": "12047"}, 2},
		{"a second scope appears", map[string]string{"CITY_STALE_WISPS": "12047", "API_STALE_WISPS": "600"}, 3},
		{"the second scope drifts", map[string]string{"CITY_STALE_WISPS": "12100", "API_STALE_WISPS": "640"}, 3},
		{"a clean run resolves the incident", nil, 3},
		{"the backlog returns", map[string]string{"CITY_STALE_WISPS": "2047"}, 4},
	})
}

// TestReaperMailWispCountDriftStaysOneIncident: the open mail-wisp backlog
// count is fingerprinted by its order of magnitude as well.
func TestReaperMailWispCountDriftStaysOneIncident(t *testing.T) {
	f := countFingerprintFixture(t)
	f.env["BD_BACKUP_STATUS_JSON"] = freshBackupStatusJSON(time.Hour, false)
	f.env["GC_REAPER_MAIL_ALERT_THRESHOLD"] = "100"
	runCountFingerprintSteps(t, f, []countFingerprintStep{
		{"250 open mail wisps", map[string]string{"CITY_MAIL_WISPS": "250"}, 1},
		{"251 open mail wisps", map[string]string{"CITY_MAIL_WISPS": "251"}, 1},
		{"2500 open mail wisps", map[string]string{"CITY_MAIL_WISPS": "2500"}, 2},
	})
}

// TestReaperCountDriftKeepsTheAgeNormalization: a count that drifts together
// with a backup that only grows older is still one incident, so folding the
// count into the fingerprint did not displace the age=<N>s normalization, and
// the two anomalies are normalized independently.
func TestReaperCountDriftKeepsTheAgeNormalization(t *testing.T) {
	f := countFingerprintFixture(t)
	steps := []countFingerprintStep{
		{"stale backup, 2047 wisps", map[string]string{"BD_BACKUP_STATUS_JSON": freshBackupStatusJSON(48*time.Hour, false), "CITY_STALE_WISPS": "2047"}, 1},
		{"older backup, 2049 wisps", map[string]string{"BD_BACKUP_STATUS_JSON": freshBackupStatusJSON(49*time.Hour, false), "CITY_STALE_WISPS": "2049"}, 1},
		{"no backup at all, 2049 wisps", map[string]string{"BD_BACKUP_STATUS_JSON": `{"backup":{},"dolt":{"configured":false}}`, "CITY_STALE_WISPS": "2049"}, 2},
	}
	runCountFingerprintSteps(t, f, steps)
}
