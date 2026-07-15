package beads

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

func reflectDeepEqual(a, b any) bool { return reflect.DeepEqual(a, b) }

// diffEndStates renders a human-readable field-by-field diff of two end-states
// for test failure output.
func diffEndStates(want, got mergeEndState) string {
	var b strings.Builder
	diffBeadMap(&b, "beads", want.beads, got.beads)
	diffDepMap(&b, "deps", want.deps, got.deps)
	diffStructSet(&b, "dirty", want.dirty, got.dirty)
	diffU64Map(&b, "beadSeq", want.beadSeq, got.beadSeq)
	diffTimeMap(&b, "localBeadAt", want.localBeadAt, got.localBeadAt)
	diffU64Map(&b, "deletedSeq", want.deletedSeq, got.deletedSeq)
	if want.depsComplete != got.depsComplete {
		fmt.Fprintf(&b, "  depsComplete: want=%v got=%v\n", want.depsComplete, got.depsComplete)
	}
	if want.state != got.state {
		fmt.Fprintf(&b, "  state: want=%v got=%v\n", want.state, got.state)
	}
	if !want.lastFreshAt.Equal(got.lastFreshAt) {
		fmt.Fprintf(&b, "  lastFreshAt: want=%v got=%v\n", want.lastFreshAt, got.lastFreshAt)
	}
	if want.mutationSeq != got.mutationSeq {
		fmt.Fprintf(&b, "  mutationSeq: want=%v got=%v\n", want.mutationSeq, got.mutationSeq)
	}
	if want.primeErr != got.primeErr {
		fmt.Fprintf(&b, "  primeErr: want=%q got=%q\n", want.primeErr, got.primeErr)
	}
	if want.syncFailures != got.syncFailures {
		fmt.Fprintf(&b, "  syncFailures: want=%v got=%v\n", want.syncFailures, got.syncFailures)
	}
	if want.statsAdds != got.statsAdds {
		fmt.Fprintf(&b, "  stats.Adds: want=%v got=%v\n", want.statsAdds, got.statsAdds)
	}
	if want.statsRemoves != got.statsRemoves {
		fmt.Fprintf(&b, "  stats.Removes: want=%v got=%v\n", want.statsRemoves, got.statsRemoves)
	}
	if want.statsUpdates != got.statsUpdates {
		fmt.Fprintf(&b, "  stats.Updates: want=%v got=%v\n", want.statsUpdates, got.statsUpdates)
	}
	if !want.statsLastReconcileAt.Equal(got.statsLastReconcileAt) {
		fmt.Fprintf(&b, "  stats.LastReconcileAt: want=%v got=%v\n", want.statsLastReconcileAt, got.statsLastReconcileAt)
	}
	if !want.statsLastFreshAt.Equal(got.statsLastFreshAt) {
		fmt.Fprintf(&b, "  stats.LastFreshAt: want=%v got=%v\n", want.statsLastFreshAt, got.statsLastFreshAt)
	}
	if b.Len() == 0 {
		return "  (no field-level diff detected — check reflect.DeepEqual edge cases)\n"
	}
	return b.String()
}

func sortedKeysAny[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func diffBeadMap(b *strings.Builder, label string, want, got map[string]Bead) {
	keys := unionKeysBead(want, got)
	for _, k := range keys {
		wv, wok := want[k]
		gv, gok := got[k]
		switch {
		case wok && !gok:
			fmt.Fprintf(b, "  %s[%q]: want present, got absent\n", label, k)
		case !wok && gok:
			fmt.Fprintf(b, "  %s[%q]: want absent, got present\n", label, k)
		case wok && gok && !reflect.DeepEqual(wv, gv):
			fmt.Fprintf(b, "  %s[%q]: bead differs\n    want=%+v\n    got =%+v\n", label, k, wv, gv)
		}
	}
}

func diffDepMap(b *strings.Builder, label string, want, got map[string][]Dep) {
	keys := unionKeysDep(want, got)
	for _, k := range keys {
		wv, wok := want[k]
		gv, gok := got[k]
		switch {
		case wok && !gok:
			fmt.Fprintf(b, "  %s[%q]: want present (%v), got absent\n", label, k, wv)
		case !wok && gok:
			fmt.Fprintf(b, "  %s[%q]: want absent, got present (%v)\n", label, k, gv)
		case wok && gok && !reflect.DeepEqual(wv, gv):
			fmt.Fprintf(b, "  %s[%q]: want=%v got=%v\n", label, k, wv, gv)
		}
	}
}

func diffStructSet(b *strings.Builder, label string, want, got map[string]struct{}) {
	for _, k := range sortedKeysAny(want) {
		if _, ok := got[k]; !ok {
			fmt.Fprintf(b, "  %s[%q]: want present, got absent\n", label, k)
		}
	}
	for _, k := range sortedKeysAny(got) {
		if _, ok := want[k]; !ok {
			fmt.Fprintf(b, "  %s[%q]: want absent, got present\n", label, k)
		}
	}
}

func diffU64Map(b *strings.Builder, label string, want, got map[string]uint64) {
	keys := unionKeysU64(want, got)
	for _, k := range keys {
		wv, wok := want[k]
		gv, gok := got[k]
		if wok != gok || wv != gv {
			fmt.Fprintf(b, "  %s[%q]: want=(%d,present=%v) got=(%d,present=%v)\n", label, k, wv, wok, gv, gok)
		}
	}
}

func diffTimeMap(b *strings.Builder, label string, want, got map[string]time.Time) {
	keys := unionKeysTime(want, got)
	for _, k := range keys {
		wv, wok := want[k]
		gv, gok := got[k]
		if wok != gok || !wv.Equal(gv) {
			fmt.Fprintf(b, "  %s[%q]: want=(%v,present=%v) got=(%v,present=%v)\n", label, k, wv, wok, gv, gok)
		}
	}
}

func unionKeysBead(a, b map[string]Bead) []string {
	set := map[string]struct{}{}
	for k := range a {
		set[k] = struct{}{}
	}
	for k := range b {
		set[k] = struct{}{}
	}
	return sortedKeysAny(set)
}

func unionKeysDep(a, b map[string][]Dep) []string {
	set := map[string]struct{}{}
	for k := range a {
		set[k] = struct{}{}
	}
	for k := range b {
		set[k] = struct{}{}
	}
	return sortedKeysAny(set)
}

func unionKeysU64(a, b map[string]uint64) []string {
	set := map[string]struct{}{}
	for k := range a {
		set[k] = struct{}{}
	}
	for k := range b {
		set[k] = struct{}{}
	}
	return sortedKeysAny(set)
}

func unionKeysTime(a, b map[string]time.Time) []string {
	set := map[string]struct{}{}
	for k := range a {
		set[k] = struct{}{}
	}
	for k := range b {
		set[k] = struct{}{}
	}
	return sortedKeysAny(set)
}
