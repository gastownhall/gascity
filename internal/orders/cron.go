package orders

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// This is the ONE cron parser in gc. It exists because there were two, and
// they disagreed.
//
// The dispatcher used to carry a lenient matcher that understood "*", a bare
// integer, a comma list and "*/N" — and silently skipped any part it could
// not parse. `internal/doctor` grew a second, stricter parser that also
// understood ranges ("16-23") and stepped ranges ("1-5/2"), bounds-checked
// each field, and returned an error instead of swallowing one. Neither knew
// about the other, so a schedule using a range was read two different ways in
// one binary: the doctor computed a firing interval from it and reported the
// order healthy, while the dispatcher's matcher never matched it and the order
// never fired once. Nothing reported the disagreement, because "no match" and
// "cannot parse" were the same value.
//
// Both callers now parse here. Divergence is not something to re-detect later;
// it is something to make unrepresentable.

// ErrInvalidSchedule marks a cron order rejected for its schedule rather than
// for anything else about it. Order discovery DROPS an order that fails
// validation, so a caller that reports on scheduling — gc doctor's firing
// check — needs to tell "this order's trigger is broken", which is its
// subject and must be surfaced, from an unrelated rejection that another
// check already owns. Without the distinction, adding schedule validation
// merely moves a visible failure into a log line.
var ErrInvalidSchedule = errors.New("invalid schedule")

// cronFieldBounds are the legal value ranges per field, in schedule order.
// Day-of-week accepts 7 as a second spelling of Sunday, as cron does; matching
// normalizes it (see cronField.matches).
var cronFieldBounds = [5]struct {
	name     string
	min, max int
}{
	{"minute", 0, 59},
	{"hour", 0, 23},
	{"day-of-month", 1, 31},
	{"month", 1, 12},
	{"day-of-week", 0, 7},
}

// cronField is one parsed field: the union of its comma-separated parts.
type cronField struct {
	parts []cronPart
	isDOW bool
}

// cronPart is one comma-separated term, normalized to a stepped range.
// A bare "5" is the range 5-5 step 1; "*" is the field's full bounds.
type cronPart struct {
	lo, hi, step int
}

func (p cronPart) matches(v int) bool {
	return v >= p.lo && v <= p.hi && (v-p.lo)%p.step == 0
}

func (f cronField) matches(v int) bool {
	for _, p := range f.parts {
		if p.matches(v) {
			return true
		}
	}
	// Sunday is both 0 and 7 in cron. time.Weekday only ever gives 0, so a
	// field written "7" or "1-7" would otherwise never match a Sunday — the
	// same silent never-fires this parser exists to eliminate.
	if f.isDOW && v == 0 {
		for _, p := range f.parts {
			if p.matches(7) {
				return true
			}
		}
	}
	return false
}

// CronSchedule is a parsed 5-field cron expression. The zero value is not
// usable; obtain one from ParseCronSchedule.
type CronSchedule struct {
	fields [5]cronField
}

// MatchesAt reports whether the schedule fires at t's wall clock. t must
// already be in the location the schedule is evaluated in; this reads only
// t's calendar components.
//
// KNOWN DIVERGENCE FROM POSIX CRON, deliberately preserved here: all five
// fields are ANDed. Vixie and POSIX cron special-case day-of-month against
// day-of-week — when BOTH are restricted (neither is "*"), the schedule fires
// when EITHER matches, not both. gc has always ANDed, in both of the parsers
// this file replaces, so changing it here would be a second behavior change
// riding along with a fix for something else. It is written down rather than
// left to be rediscovered: an undocumented divergence is the same failure this
// file exists to remove, one level up.
func (s CronSchedule) MatchesAt(t time.Time) bool {
	return s.fields[0].matches(t.Minute()) &&
		s.fields[1].matches(t.Hour()) &&
		s.fields[2].matches(t.Day()) &&
		s.fields[3].matches(int(t.Month())) &&
		s.fields[4].matches(int(t.Weekday()))
}

// ParseCronSchedule parses "minute hour day-of-month month day-of-week".
//
// Each field is a comma-separated list of: "*", an integer, a range "A-B", or
// either of the last two with a step suffix ("*/N", "A-B/N"). Steps count from
// the start of the range, so "*/15" on minutes is 0,15,30,45 and "*/2" on
// day-of-month is 1,3,5,... — the field's minimum, not zero.
//
// An unparseable or out-of-bounds field is an error. That is the point: the
// previous matcher returned "did not match" for input it could not read, which
// is indistinguishable from a schedule that legitimately does not fire now, so
// a typo cost a silent order rather than a loud failure.
func ParseCronSchedule(schedule string) (CronSchedule, error) {
	var s CronSchedule
	raw := strings.Fields(schedule)
	if len(raw) != 5 {
		return s, fmt.Errorf("want 5 fields, got %d", len(raw))
	}
	for i, b := range cronFieldBounds {
		f, err := parseCronField(raw[i], b.min, b.max)
		if err != nil {
			return s, fmt.Errorf("%s field %q: %w", b.name, raw[i], err)
		}
		f.isDOW = i == 4
		s.fields[i] = f
	}
	return s, nil
}

func parseCronField(field string, fieldMin, fieldMax int) (cronField, error) {
	var f cronField
	if strings.TrimSpace(field) == "" {
		return f, fmt.Errorf("empty field")
	}
	for _, raw := range strings.Split(field, ",") {
		p, err := parseCronPart(strings.TrimSpace(raw), fieldMin, fieldMax)
		if err != nil {
			return f, err
		}
		f.parts = append(f.parts, p)
	}
	return f, nil
}

func parseCronPart(part string, fieldMin, fieldMax int) (cronPart, error) {
	if part == "" {
		return cronPart{}, fmt.Errorf("empty term")
	}
	rangeSpec, stepSpec, hasStep := strings.Cut(part, "/")
	step := 1
	if hasStep {
		// A step needs something to step over. Cron rejects "1/2" — a step on
		// a single value — and accepting it would silently turn a typo into a
		// schedule that fires on exactly one value and looks intentional.
		if rangeSpec != "*" && !strings.Contains(rangeSpec, "-") {
			return cronPart{}, fmt.Errorf("step on the single value %q: use a range or \"*\"", rangeSpec)
		}
		n, err := parseCronInt(stepSpec)
		if err != nil {
			return cronPart{}, fmt.Errorf("step %q is not a number", stepSpec)
		}
		if n <= 0 {
			return cronPart{}, fmt.Errorf("step %d must be positive", n)
		}
		step = n
	}
	lo, hi, err := parseCronRange(strings.TrimSpace(rangeSpec), fieldMin, fieldMax)
	if err != nil {
		return cronPart{}, err
	}
	return cronPart{lo: lo, hi: hi, step: step}, nil
}

// parseCronInt accepts only unsigned decimal digits. strconv.Atoi alone would
// take "+5" and "-5"; cron does not, and a signed token in a schedule is a
// typo that should be reported rather than quietly given a meaning.
func parseCronInt(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty number")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%q is not a number", s)
		}
	}
	return strconv.Atoi(s)
}

func parseCronRange(spec string, fieldMin, fieldMax int) (int, int, error) {
	if spec == "*" {
		return fieldMin, fieldMax, nil
	}
	if lo, hi, ok := strings.Cut(spec, "-"); ok {
		start, err := parseCronInt(lo)
		if err != nil {
			return 0, 0, fmt.Errorf("range start %q is not a number", lo)
		}
		end, err := parseCronInt(hi)
		if err != nil {
			return 0, 0, fmt.Errorf("range end %q is not a number", hi)
		}
		if start > end {
			return 0, 0, fmt.Errorf("range %d-%d is inverted", start, end)
		}
		if start < fieldMin || end > fieldMax {
			return 0, 0, fmt.Errorf("range %d-%d is outside %d-%d", start, end, fieldMin, fieldMax)
		}
		return start, end, nil
	}
	v, err := parseCronInt(spec)
	if err != nil {
		return 0, 0, fmt.Errorf("%q is not a number", spec)
	}
	if v < fieldMin || v > fieldMax {
		return 0, 0, fmt.Errorf("%d is outside %d-%d", v, fieldMin, fieldMax)
	}
	return v, v, nil
}
