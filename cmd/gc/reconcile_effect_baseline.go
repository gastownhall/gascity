package main

// A8's baseline write (CONTRACT v5 M2; C7c): one row write behind the
// baseline premise.

// redecideBaseline is redecideRow; a soft reload's cancel of a config-drift
// request carries legacy's "cancel" drain transition.
func redecideBaseline(v txView) txStep {
	step := redecideRow(v)
	if cancelsStop(step.Write) {
		step.Facts.Transition = &drainTransition{Name: v.Row.SessionNameMetadata, Reason: drainConfigDrift, Transition: "cancel"}
	}
	return step
}

// baselineSections are A8's write: behind the baseline premise
// (premiseBaseline), re-decided on the fresh row.
var baselineSections = []section{{Premise: premiseBaseline, Decide: redecideBaseline}}
