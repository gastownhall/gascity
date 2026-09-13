package waiverclock

import (
	"strings"
	"testing"
	"time"
)

func day(year int, month time.Month, dayOfMonth int) time.Time {
	return time.Date(year, month, dayOfMonth, 0, 0, 0, 0, time.UTC)
}

func TestExpiredUsesWholeDayGranularity(t *testing.T) {
	expires := day(2026, time.September, 7)
	for _, tc := range []struct {
		name string
		now  time.Time
		want bool
	}{
		{"day before", day(2026, time.September, 6), false},
		{"midnight on the expiry day", expires, false},
		{"late on the expiry day", time.Date(2026, time.September, 7, 23, 59, 59, 0, time.UTC), false},
		{"the day after", day(2026, time.September, 8), true},
		{"long after", day(2026, time.October, 1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Expired(expires, tc.now); got != tc.want {
				t.Fatalf("Expired(%s, %s) = %t, want %t",
					expires.Format(time.RFC3339), tc.now.Format(time.RFC3339), got, tc.want)
			}
		})
	}
}

// TestExpiredIsTimezoneIndependent pins that a non-UTC now is normalized before
// the day comparison, so a machine in a positive offset zone cannot expire a
// waiver a day early.
func TestExpiredIsTimezoneIndependent(t *testing.T) {
	expires := day(2026, time.September, 7)
	plus14 := time.FixedZone("plus14", 14*60*60)
	now := time.Date(2026, time.September, 8, 10, 0, 0, 0, plus14) // 2026-09-07T20:00Z
	if Expired(expires, now) {
		t.Fatalf("Expired(%s, %s) = true; a waiver must survive its whole UTC day",
			expires.Format(time.RFC3339), now.Format(time.RFC3339))
	}
}

func TestFromEnv(t *testing.T) {
	for _, tc := range []struct {
		value   string
		unset   bool
		want    Mode
		wantErr bool
	}{
		{unset: true, want: ModeGrace},
		{value: "", want: ModeGrace},
		{value: "grace", want: ModeGrace},
		{value: "strict", want: ModeStrict},
		{value: "STRICT", want: ModeStrict},
		{value: " strict ", want: ModeStrict},
		{value: "yes", wantErr: true},
		{value: "1", wantErr: true},
		{value: "off", wantErr: true},
	} {
		name := tc.value
		if tc.unset {
			name = "<unset>"
		}
		t.Run(name, func(t *testing.T) {
			if tc.unset {
				t.Setenv(EnvVar, "")
				// t.Setenv cannot unset; exercise the empty-string path, which
				// FromEnv must treat identically to an absent variable.
			} else {
				t.Setenv(EnvVar, tc.value)
			}
			got, err := FromEnv()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("FromEnv() with %s=%q returned mode %v, want an error", EnvVar, tc.value, got)
				}
				if !strings.Contains(err.Error(), EnvVar) {
					t.Fatalf("FromEnv() error %q does not name %s", err, EnvVar)
				}
				return
			}
			if err != nil {
				t.Fatalf("FromEnv() with %s=%q: %v", EnvVar, tc.value, err)
			}
			if got != tc.want {
				t.Fatalf("FromEnv() with %s=%q = %v, want %v", EnvVar, tc.value, got, tc.want)
			}
		})
	}
}

func sampleExpiry(expires time.Time) Expiry {
	return Expiry{
		Label:   `entry "runtime.builtin.ssh" constructor internal/runtime/ssh.NewSeamBacked contract runtime.Provider`,
		Owner:   "ga-80po0c.3",
		Expires: expires,
	}
}

func TestCheckClassification(t *testing.T) {
	expires := day(2026, time.September, 7)
	for _, tc := range []struct {
		name         string
		now          time.Time
		mode         Mode
		wantFatal    int
		wantWarnings int
	}{
		{"far from expiry stays silent", day(2026, time.August, 1), ModeGrace, 0, 0},
		{"far from expiry stays silent in strict", day(2026, time.August, 1), ModeStrict, 0, 0},
		{"inside the warn window warns", day(2026, time.August, 28), ModeGrace, 0, 1},
		{"on the expiry day warns", expires, ModeGrace, 0, 1},
		{"lapsed inside grace warns", day(2026, time.September, 8), ModeGrace, 0, 1},
		{"last grace day warns", day(2026, time.September, 21), ModeGrace, 0, 1},
		{"past grace is fatal", day(2026, time.September, 22), ModeGrace, 1, 0},
		{"lapsed is fatal in strict", day(2026, time.September, 8), ModeStrict, 1, 0},
		{"strict does not fault an unexpired waiver", expires, ModeStrict, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Check([]Expiry{sampleExpiry(expires)}, tc.now, tc.mode)
			if len(got.Fatal) != tc.wantFatal {
				t.Fatalf("Fatal = %d %v, want %d", len(got.Fatal), got.Fatal, tc.wantFatal)
			}
			if len(got.Warnings) != tc.wantWarnings {
				t.Fatalf("Warnings = %d %v, want %d", len(got.Warnings), got.Warnings, tc.wantWarnings)
			}
		})
	}
}

// TestCheckMessageRoutesToTheOwner pins the parts of the message a blocked
// engineer needs: whose waiver it is, how to read that bead, when the grace
// window closes, and how to reproduce the strict verdict. The 2026-08-26
// incident produced "waiver owned by ga-uz5t3a expired 2026-09-07" and nothing
// else, which is why the fleet cleared it three times without the owner hearing.
func TestCheckMessageRoutesToTheOwner(t *testing.T) {
	expires := day(2026, time.September, 7)
	report := Check([]Expiry{sampleExpiry(expires)}, day(2026, time.September, 22), ModeGrace)
	if len(report.Fatal) != 1 {
		t.Fatalf("Fatal = %v, want exactly one finding", report.Fatal)
	}
	message := report.Fatal[0]
	for _, want := range []string{
		`entry "runtime.builtin.ssh"`,
		"ga-80po0c.3",
		"expired 2026-09-07",
		"bd show ga-80po0c.3",
		"belongs to the waiver owner",
		EnvVar + "=strict",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("message is missing %q:\n%s", want, message)
		}
	}
}

func TestCheckReportsTheFleetFatalDate(t *testing.T) {
	expires := day(2026, time.September, 7)
	report := Check([]Expiry{sampleExpiry(expires)}, day(2026, time.September, 8), ModeGrace)
	if len(report.Warnings) != 1 {
		t.Fatalf("Warnings = %v, want exactly one", report.Warnings)
	}
	// Expiry day + 14d grace: the last tolerated day is 2026-09-21.
	if !strings.Contains(report.Warnings[0], "2026-09-21") {
		t.Fatalf("warning does not name the fleet-fatal date 2026-09-21:\n%s", report.Warnings[0])
	}
}

func TestCheckHandlesEveryItemIndependently(t *testing.T) {
	now := day(2026, time.September, 22)
	items := []Expiry{
		{Label: "lapsed-past-grace", Owner: "ga-a", Expires: day(2026, time.September, 7)},
		{Label: "lapsed-in-grace", Owner: "ga-b", Expires: day(2026, time.September, 15)},
		{Label: "approaching", Owner: "ga-c", Expires: day(2026, time.September, 30)},
		{Label: "distant", Owner: "ga-d", Expires: day(2027, time.January, 1)},
	}
	got := Check(items, now, ModeGrace)
	if len(got.Fatal) != 1 || !strings.Contains(got.Fatal[0], "lapsed-past-grace") {
		t.Fatalf("Fatal = %v, want only lapsed-past-grace", got.Fatal)
	}
	if len(got.Warnings) != 2 {
		t.Fatalf("Warnings = %v, want lapsed-in-grace and approaching", got.Warnings)
	}
}

func TestCheckIgnoresZeroExpiry(t *testing.T) {
	// A missing expiry is a structural defect the owning validator reports; the
	// clock must not also fault it, or one authoring mistake yields two findings.
	got := Check([]Expiry{{Label: "no-date", Owner: "ga-a"}}, day(2026, time.September, 22), ModeStrict)
	if len(got.Fatal) != 0 || len(got.Warnings) != 0 {
		t.Fatalf("Check on a zero expiry returned %+v, want an empty report", got)
	}
}

func TestModeString(t *testing.T) {
	if ModeGrace.String() != "grace" || ModeStrict.String() != "strict" {
		t.Fatalf("Mode.String() = %q/%q, want grace/strict", ModeGrace, ModeStrict)
	}
}
