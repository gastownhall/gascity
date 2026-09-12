package orders

import (
	"strings"
	"testing"
	"time"
)

// matchField evaluates one field in isolation by wildcarding the other four.
func matchField(t *testing.T, index int, field string, ts time.Time) bool {
	t.Helper()
	parts := []string{"*", "*", "*", "*", "*"}
	parts[index] = field
	s, err := ParseCronSchedule(strings.Join(parts, " "))
	if err != nil {
		t.Fatalf("ParseCronSchedule(%q) error: %v", strings.Join(parts, " "), err)
	}
	return s.MatchesAt(ts)
}

const (
	fieldMinute = iota
	fieldHour
	fieldDOM
	fieldMonth
	fieldDOW
)

// Wednesday, 2026-09-02.
func at(t *testing.T, hour, minute int) time.Time {
	t.Helper()
	return time.Date(2026, 9, 2, hour, minute, 0, 0, time.UTC)
}

func TestParseCronScheduleFieldForms(t *testing.T) {
	tests := []struct {
		name  string
		index int
		field string
		hour  int
		min   int
		want  bool
	}{
		// Forms the pre-fix matcher already handled — kept so this change
		// cannot quietly alter them.
		{"star", fieldMinute, "*", 0, 5, true},
		{"exact hit", fieldMinute, "5", 0, 5, true},
		{"exact miss", fieldMinute, "5", 0, 3, false},
		{"list hit", fieldMinute, "1,3,5", 0, 3, true},
		{"list miss", fieldMinute, "1,3,5", 0, 2, false},
		{"step hit", fieldMinute, "*/15", 0, 30, true},
		{"step miss", fieldMinute, "*/15", 0, 31, false},

		// Ranges: parsed clean and never matched before this change.
		{"range low edge", fieldHour, "16-23", 16, 0, true},
		{"range middle", fieldHour, "16-23", 18, 0, true},
		{"range high edge", fieldHour, "16-23", 23, 0, true},
		{"range below", fieldHour, "16-23", 15, 0, false},
		{"range dow hit", fieldDOW, "1-5", 0, 0, true}, // Wednesday = 3
		{"range dow miss", fieldDOW, "4-5", 0, 0, false},
		{"stepped range hit", fieldHour, "16-23/2", 18, 0, true},
		{"stepped range miss", fieldHour, "16-23/2", 19, 0, false},
		{"range inside list", fieldHour, "1,16-23", 20, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchField(t, tt.index, tt.field, at(t, tt.hour, tt.min)); got != tt.want {
				t.Errorf("field %q matched = %v, want %v", tt.field, got, tt.want)
			}
		})
	}
}

// A step counts from the start of its range, not from zero. The two agree on
// minute and hour (minimum 0) and diverge on day-of-month and month (minimum
// 1), where the pre-fix matcher's value%step gave even days for "*/2" while
// cron gives odd ones. internal/doctor already used the start-of-range
// reading, so unifying on it also settles a doctor/dispatcher disagreement.
func TestParseCronScheduleStepAnchorsAtFieldMinimum(t *testing.T) {
	for _, tc := range []struct {
		day  int
		want bool
	}{{1, true}, {2, false}, {3, true}, {4, false}} {
		ts := time.Date(2026, 9, tc.day, 0, 0, 0, 0, time.UTC)
		if got := matchField(t, fieldDOM, "*/2", ts); got != tc.want {
			t.Errorf("day-of-month */2 on day %d = %v, want %v", tc.day, got, tc.want)
		}
	}
}

// Cron spells Sunday both 0 and 7; time.Weekday only ever returns 0, so a
// field written "7" has to be normalized or it never matches a Sunday.
func TestParseCronScheduleSundayIsZeroOrSeven(t *testing.T) {
	sunday := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	if sunday.Weekday() != time.Sunday {
		t.Fatalf("fixture is not a Sunday: %s", sunday.Weekday())
	}
	for _, field := range []string{"0", "7", "1-7", "6-7"} {
		if !matchField(t, fieldDOW, field, sunday) {
			t.Errorf("day-of-week %q did not match Sunday", field)
		}
	}
	if matchField(t, fieldDOW, "1-5", sunday) {
		t.Error("day-of-week 1-5 matched Sunday")
	}
}

// Unreadable input must be an error, not a field that silently never matches.
func TestParseCronScheduleRejectsMalformed(t *testing.T) {
	tests := []struct {
		schedule string
		wantErr  string
	}{
		{"* * * *", "want 5 fields"},
		{"* * * * * *", "want 5 fields"},
		{"60 * * * *", "outside 0-59"},
		{"* 24 * * *", "outside 0-23"},
		{"* * 0 * *", "outside 1-31"},
		{"* * * 13 *", "outside 1-12"},
		{"* * * * 8", "outside 0-7"},
		{"*/0 * * * *", "must be positive"},
		{"*/x * * * *", "not a number"},
		{"* 23-16 * * *", "inverted"},
		{"* 16-2x * * *", "not a number"},
		{"* MON * * *", "not a number"},
		{"* * * * ", "want 5 fields"},
		{"1,,3 * * * *", "empty term"},
	}
	for _, tt := range tests {
		t.Run(tt.schedule, func(t *testing.T) {
			_, err := ParseCronSchedule(tt.schedule)
			if err == nil {
				t.Fatalf("ParseCronSchedule(%q) = nil error, want one", tt.schedule)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// The regression this change exists for, end to end: a real schedule from a
// live city that Validate accepted and checkCron never fired.
func TestCronRangeScheduleValidatesAndFires(t *testing.T) {
	a := Order{Name: "weekday-sweep", Trigger: "cron", Schedule: "*/15 16-23 * * 1-5", Formula: "x"}
	if err := Validate(a); err != nil {
		t.Fatalf("Validate(%q) = %v, want nil", a.Schedule, err)
	}
	res := checkCron(a, at(t, 18, 30), neverRan)
	if !res.Due {
		t.Fatalf("Due = false (%s), want true — 18:30 Wednesday is inside 16-23 on 1-5", res.Reason)
	}
}

func TestValidateRejectsUnreadableSchedule(t *testing.T) {
	a := Order{Name: "typo", Trigger: "cron", Schedule: "*/15 16-2x * * 1-5", Formula: "x"}
	err := Validate(a)
	if err == nil {
		t.Fatal("Validate = nil, want an error naming the bad field")
	}
	if !strings.Contains(err.Error(), "invalid schedule") || !strings.Contains(err.Error(), "hour") {
		t.Errorf("error = %q, want it to name the invalid schedule and the hour field", err)
	}
}

// An unreadable schedule that somehow reaches the dispatcher must say so,
// rather than reporting the same "not matched" a healthy idle order reports.
func TestCheckCronReportsUnreadableSchedule(t *testing.T) {
	a := Order{Name: "typo", Trigger: "cron", Schedule: "*/15 16-2x * * 1-5", Formula: "x"}
	res := checkCron(a, at(t, 18, 30), neverRan)
	if res.Due {
		t.Fatal("Due = true, want false")
	}
	if !strings.Contains(res.Reason, "bad cron schedule") {
		t.Errorf("Reason = %q, want it to report a bad schedule", res.Reason)
	}
}

// A step needs a range to step over, and cron numbers are unsigned. Both forms
// parsed clean before and were given a meaning nobody wrote: "1/2" fired on
// exactly one value, and strconv.Atoi quietly accepted "+5".
func TestParseCronScheduleRejectsStepOnScalarAndSignedNumbers(t *testing.T) {
	for _, tt := range []struct {
		schedule string
		wantErr  string
	}{
		{"1/2 * * * *", "step on the single value"},
		{"+5 * * * *", "not a number"},
		{"-5 * * * *", "not a number"},
		{"*/+2 * * * *", "not a number"},
		{"* +1-5 * * *", "not a number"},
	} {
		t.Run(tt.schedule, func(t *testing.T) {
			_, err := ParseCronSchedule(tt.schedule)
			if err == nil {
				t.Fatalf("ParseCronSchedule(%q) = nil error, want one", tt.schedule)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}
