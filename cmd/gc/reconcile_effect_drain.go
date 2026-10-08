package main

import "strings"

// The drain effects (CONTRACT v5 D1, D2): a cancel or a void is the row
// write of its patch, re-decided inside the CAS (redecideRow), whose landing
// records legacy's drain transition. The begin's effect is C6a2's.

// drainClearSections are a cancel's or a void's one section.
var drainClearSections = []section{{Decide: redecideDrainClear}}

// redecideDrainClear is redecideRow; its write carries legacy's "cancel"
// drain transition, a fact only once the write lands.
func redecideDrainClear(v txView) txStep {
	step := redecideRow(v)
	if len(step.Write) > 0 {
		_, reason, _ := strings.Cut(v.It.Reason, ":")
		step.Facts.Transition = &drainTransition{Name: v.Row.SessionNameMetadata, Reason: reason, Transition: "cancel"}
	}
	return step
}
