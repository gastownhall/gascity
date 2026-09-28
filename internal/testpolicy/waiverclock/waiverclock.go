// Package waiverclock defines the one fleet-wide policy for how dated
// test-policy waivers are enforced against the wall clock.
//
// Two ledgers carry dated waivers: the runtime provider ledger
// (internal/testutil/providerledger) and the resource census
// (internal/testpolicy/resourcecensus). Both are untagged, both run in the
// unit-core job, and that job runs in .githooks/pre-push. So an expiry date
// passing turns every Go-touching push in the fleet red with no code change
// involved. That happened on 2026-08-12 and again on 2026-08-26, and each time
// it was cleared by whoever happened to be blocked rather than by the waiver's
// owner.
//
// The fix is not to weaken the ratchet. It is that an expiry should fail the
// OWNER, not every bystander. This package draws the line: structural defects
// stay fatal everywhere, because they can only appear with a code change, while
// a lapse gets a bounded grace period during which it warns and names its owner,
// and a strict mode that the owner's own lanes run.
//
// Both ledgers share these semantics so they cannot drift apart the way a shared
// date already did.
package waiverclock

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// EnvVar selects the enforcement mode. Unset means ModeGrace, so a lane that
// scrubs the environment fails safe rather than silently strict.
const EnvVar = "GC_WAIVER_CLOCK"

// Mode selects how a lapsed waiver is enforced.
type Mode int

const (
	// ModeGrace is the fleet default: a lapse inside the grace window warns and
	// names its owner; past the window it is fatal everywhere. This keeps the
	// ratchet's teeth while bounding what a bystander pays for someone else's
	// missed date.
	ModeGrace Mode = iota
	// ModeStrict makes any lapse fatal on the day it happens. Only the owner's
	// lanes run this, and they are all scheduled: scripts/waiver-clock-audit,
	// reached by the nightly waiver-clock job and by the maintainer city's audit
	// order. Nothing on the commit or push path may set it, or a calendar
	// rollover blocks pushes carrying no code change all over again.
	ModeStrict
)

// String renders the mode as it is spelled in EnvVar.
func (m Mode) String() string {
	switch m {
	case ModeStrict:
		return "strict"
	default:
		return "grace"
	}
}

// Grace is how long a lapsed waiver warns before it fails every lane.
const Grace = 14 * 24 * time.Hour

// WarnAhead is how long before its expiry a waiver starts warning, so the owner
// hears about it while there is still time to act.
const WarnAhead = 14 * 24 * time.Hour

// FromEnv reads the enforcement mode from EnvVar. An unset or empty value is
// ModeGrace. An unrecognized value is an error rather than a fallback, so a typo
// cannot quietly weaken the gate.
func FromEnv() (Mode, error) {
	switch value := strings.ToLower(strings.TrimSpace(os.Getenv(EnvVar))); value {
	case "", "grace":
		return ModeGrace, nil
	case "strict":
		return ModeStrict, nil
	default:
		return ModeGrace, fmt.Errorf("%s=%q is not a valid waiver clock mode (want %q or %q)",
			EnvVar, value, ModeGrace, ModeStrict)
	}
}

// Expiry is one dated waiver as the clock policy sees it. Label is the caller's
// own problem prefix, so each ledger keeps its own vocabulary.
type Expiry struct {
	Label   string
	Owner   string
	Expires time.Time
}

// Report separates what must fail the caller from what must only be seen.
type Report struct {
	Fatal    []string
	Warnings []string
}

// UTCDay truncates a time to the UTC day it falls in.
func UTCDay(value time.Time) time.Time {
	value = value.UTC()
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}

// Expired reports whether a waiver dated expires has lapsed at now. A waiver is
// valid through the whole UTC day it names, which is what the rendered
// YYYY-MM-DD tables in TESTING.md already claim.
func Expired(expires, now time.Time) bool {
	return UTCDay(now).After(UTCDay(expires))
}

// FleetFatalDay is the last day a lapsed waiver only warns under ModeGrace.
func FleetFatalDay(expires time.Time) time.Time {
	return UTCDay(expires).Add(Grace)
}

// Check classifies every expiry at now under mode. A zero expiry is skipped: a
// missing date is a structural defect the owning validator already reports, and
// faulting it here would turn one authoring mistake into two findings.
func Check(items []Expiry, now time.Time, mode Mode) Report {
	var report Report
	for _, item := range items {
		if item.Expires.IsZero() {
			continue
		}
		switch {
		case Expired(item.Expires, now):
			fatalDay := FleetFatalDay(item.Expires)
			if mode == ModeStrict || UTCDay(now).After(fatalDay) {
				report.Fatal = append(report.Fatal, item.render("expired", fatalDay))
				continue
			}
			report.Warnings = append(report.Warnings, item.render("expired", fatalDay))
		case !UTCDay(now).Add(WarnAhead).Before(UTCDay(item.Expires)):
			report.Warnings = append(report.Warnings, item.render("expires", FleetFatalDay(item.Expires)))
		}
	}
	return report
}

// render builds the message a blocked engineer actually reads. It has to answer
// three questions the 2026-08-26 wording did not: whose waiver this is, how to
// reach that owner, and how long the fleet tolerates it.
func (e Expiry) render(verb string, fatalDay time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: waiver owned by %s %s %s",
		e.Label, e.Owner, verb, e.Expires.UTC().Format("2006-01-02"))
	fmt.Fprintf(&b, "; fleet-fatal after %s", fatalDay.Format("2006-01-02"))
	b.WriteString("; this failure belongs to the waiver owner, not to your change")
	fmt.Fprintf(&b, "; context: bd show %s", e.Owner)
	fmt.Fprintf(&b, "; reproduce strictly: %s=strict", EnvVar)
	return b.String()
}
