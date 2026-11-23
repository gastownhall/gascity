package main

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/extmsg"
)

// extmsgTranscriptSequencingRemedy says which stores allocate transcript
// sequences atomically.
const extmsgTranscriptSequencingRemedy = "atomic allocation needs a messaging store with pinned-id creates and revision-fenced writes: " +
	"the native Dolt, SQLite and file stores have both, and a store served through bd has them once bd supports --if-revision, " +
	"which no released bd does yet; until then every append still records its entry, unguarded"

// extmsgTranscriptCapableMessage is the check's OK message for a store that
// passes the probe. It reports the capability, not that every append uses it:
// the probe writes nothing, so it cannot see a pinned create the store refuses
// only when an append runs it.
const extmsgTranscriptCapableMessage = "the messaging store supports atomic transcript sequence allocation; " +
	"an append whose pinned create it refuses (a bd whose version cannot be read, " +
	"or a conversation gc storage migrate carried in under its work-store ID) still runs unguarded, " +
	"and the first such append is logged"

// extmsgTranscriptDoctorCheck reports whether the city's messaging store can
// allocate external-messaging transcript sequences atomically
// (extmsg.ProbeTranscriptSequencing). A store that cannot appends unguarded,
// which the check warns about once the store holds a transcript state.
type extmsgTranscriptDoctorCheck struct {
	// open opens the city store's messaging class, with the closer of the
	// city store it routes through.
	open func() (beads.MailStore, func(), error)
}

// newExtmsgTranscriptDoctorCheck builds the check over the city store.
func newExtmsgTranscriptDoctorCheck(cityPath string, cfg *config.City) *extmsgTranscriptDoctorCheck {
	return &extmsgTranscriptDoctorCheck{
		open: func() (beads.MailStore, func(), error) {
			store, err := openCityStoreAtWithConfig(cityPath, cfg)
			if err != nil {
				return beads.MailStore{}, nil, err
			}
			return cliMailStore(store, cfg, cityPath), func() { _ = closeBeadStoreHandle(store) }, nil
		},
	}
}

// Name implements doctor.Check.
func (*extmsgTranscriptDoctorCheck) Name() string { return "extmsg-transcript-sequencing" }

// CanFix implements doctor.Check.
func (*extmsgTranscriptDoctorCheck) CanFix() bool { return false }

// WarmupEligible implements doctor.Check.
func (*extmsgTranscriptDoctorCheck) WarmupEligible() bool { return false }

// Fix implements doctor.Check.
func (*extmsgTranscriptDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

// Run implements doctor.Check: OK when the messaging store can allocate
// transcript sequences atomically, or when it cannot but holds no transcript
// state; a Warning when it cannot and holds one, or when the store does not
// open. It runs the store's read-only capability probe and reads at most one
// transcript state; it writes nothing.
func (c *extmsgTranscriptDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK}
	mail, closeStore, err := c.open()
	if err != nil {
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("transcript sequencing not checked: opening the city store: %v", err)
		return r
	}
	defer closeStore()
	unguarded := extmsg.ProbeTranscriptSequencing(mail)
	if unguarded == nil {
		r.Message = extmsgTranscriptCapableMessage
		return r
	}
	inUse, err := extmsg.HasTranscriptState(mail)
	switch {
	case err != nil:
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("external-messaging transcript appends run unguarded (%v), and whether the city holds a transcript could not be read: %v", unguarded, err)
		r.FixHint = extmsgTranscriptSequencingRemedy
	case inUse:
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("external-messaging transcript appends run unguarded: %v; an ambiguous create, a failed head write or a writer in another process can give two entries one sequence", unguarded)
		r.FixHint = extmsgTranscriptSequencingRemedy
	default:
		r.Message = fmt.Sprintf("the city holds no external-messaging transcript; appends would run unguarded: %v", unguarded)
	}
	return r
}
