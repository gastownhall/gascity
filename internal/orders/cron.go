package orders

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// CronFieldCount is the number of whitespace-separated fields a cron schedule
// must have: minute, hour, day-of-month, month, day-of-week.
const CronFieldCount = 5

// cronFieldSpec names one cron field and the range its values may take.
type cronFieldSpec struct {
	name string
	min  int
	max  int
}

// cronFieldSpecs lists the five cron fields in schedule order. Every consumer
// — the runtime trigger matcher, order validation at discovery, and the
// doctor's order-firing check — reads schedules through this one table, so no
// two of them can disagree about what a field accepts.
var cronFieldSpecs = [CronFieldCount]cronFieldSpec{
	{name: "minute", min: 0, max: 59},
	{name: "hour", min: 0, max: 23},
	{name: "day-of-month", min: 1, max: 31},
	{name: "month", min: 1, max: 12},
	{name: "day-of-week", min: 0, max: 6},
}

// cronFieldValuesAt returns t's five cron field values in schedule order.
func cronFieldValuesAt(t time.Time) [CronFieldCount]int {
	return [CronFieldCount]int{t.Minute(), t.Hour(), t.Day(), int(t.Month()), int(t.Weekday())}
}

// CronScheduleMatchesAt reports whether a 5-field cron schedule matches t's
// wall-clock reading. It returns an error when any field is unparseable rather
// than treating it as a non-match: a schedule nobody can parse is a broken
// order, not an order that is merely not due.
func CronScheduleMatchesAt(fields []string, t time.Time) (bool, error) {
	if len(fields) != CronFieldCount {
		return false, fmt.Errorf("want %d fields, got %d", CronFieldCount, len(fields))
	}
	values := cronFieldValuesAt(t)
	for i, spec := range cronFieldSpecs {
		matched, err := CronFieldMatches(fields[i], values[i], spec.min, spec.max)
		if err != nil {
			return false, fmt.Errorf("cannot parse %s field %q: %w", spec.name, fields[i], err)
		}
		if !matched {
			return false, nil
		}
	}
	return true, nil
}

// ValidateCronSchedule checks that a schedule has the right field count and
// that every field parses within its bounds. It reports the first problem it
// finds and does not evaluate the schedule against any particular time.
func ValidateCronSchedule(schedule string) error {
	fields := strings.Fields(schedule)
	if len(fields) != CronFieldCount {
		return fmt.Errorf("want %d fields, got %d", CronFieldCount, len(fields))
	}
	for i, spec := range cronFieldSpecs {
		// Parse errors do not depend on the value being matched, so any
		// in-bounds value exercises the whole field.
		if _, err := CronFieldMatches(fields[i], spec.min, spec.min, spec.max); err != nil {
			return fmt.Errorf("cannot parse %s field %q: %w", spec.name, fields[i], err)
		}
	}
	return nil
}

// CronFieldMatches reports whether one cron field matches value, where legal
// values run from lowerBound to upperBound inclusive. Supported syntax per
// comma-separated part: "*", an integer, a range "a-b", and either form with a
// step suffix ("*/N", "a-b/N").
//
// Every part is parsed even after a match is found, so a malformed part fails
// loudly instead of hiding behind an earlier one.
func CronFieldMatches(field string, value, lowerBound, upperBound int) (bool, error) {
	if strings.TrimSpace(field) == "" {
		return false, fmt.Errorf("empty field")
	}
	matched := false
	for _, rawPart := range strings.Split(field, ",") {
		lo, hi, step, err := parseCronPart(strings.TrimSpace(rawPart), lowerBound, upperBound)
		if err != nil {
			return false, err
		}
		if value >= lo && value <= hi && (value-lo)%step == 0 {
			matched = true
		}
	}
	return matched, nil
}

// parseCronPart resolves one comma-separated part into the inclusive value
// range it covers and its step. The step is anchored at the range's lower
// bound — "*/2" on day-of-month means the 1st, 3rd, 5th, ... as standard cron
// does, not the even days.
func parseCronPart(part string, lowerBound, upperBound int) (lo, hi, step int, err error) {
	if part == "" {
		return 0, 0, 0, fmt.Errorf("empty part")
	}
	rangePart, stepPart, hasStep := strings.Cut(part, "/")
	step = 1
	if hasStep {
		parsed, atoiErr := strconv.Atoi(strings.TrimSpace(stepPart))
		if atoiErr != nil || parsed <= 0 {
			return 0, 0, 0, fmt.Errorf("invalid step")
		}
		step = parsed
	}
	lo, hi, err = parseCronRange(strings.TrimSpace(rangePart), lowerBound, upperBound)
	if err != nil {
		return 0, 0, 0, err
	}
	return lo, hi, step, nil
}

// parseCronRange resolves the pre-step portion of a part ("*", "a-b", or a
// single integer) into an inclusive range, rejecting anything outside the
// field's bounds.
func parseCronRange(rangePart string, lowerBound, upperBound int) (int, int, error) {
	switch {
	case rangePart == "*":
		return lowerBound, upperBound, nil
	case strings.Contains(rangePart, "-"):
		start, end, ok := strings.Cut(rangePart, "-")
		if !ok {
			return 0, 0, fmt.Errorf("invalid range")
		}
		lo, err := strconv.Atoi(strings.TrimSpace(start))
		if err != nil {
			return 0, 0, err
		}
		hi, err := strconv.Atoi(strings.TrimSpace(end))
		if err != nil {
			return 0, 0, err
		}
		if lo < lowerBound || hi > upperBound || lo > hi {
			return 0, 0, fmt.Errorf("range out of bounds")
		}
		return lo, hi, nil
	default:
		value, err := strconv.Atoi(rangePart)
		if err != nil {
			return 0, 0, err
		}
		if value < lowerBound || value > upperBound {
			return 0, 0, fmt.Errorf("value out of bounds")
		}
		return value, value, nil
	}
}
