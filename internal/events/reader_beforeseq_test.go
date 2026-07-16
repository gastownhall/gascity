package events

import "testing"

// TestFilterBeforeSeq pins the keyset page boundary for descending event
// walks: BeforeSeq matches strictly-below rows, composes with AfterSeq, and
// zero means no filter.
func TestFilterBeforeSeq(t *testing.T) {
	evts := []Event{{Seq: 1}, {Seq: 2}, {Seq: 3}, {Seq: 4}}

	got := ApplyFilter(evts, Filter{BeforeSeq: 3})
	if len(got) != 2 || got[0].Seq != 1 || got[1].Seq != 2 {
		t.Fatalf("BeforeSeq=3 -> %v, want seqs [1 2]", got)
	}
	if got := ApplyFilter(evts, Filter{BeforeSeq: 0}); len(got) != 4 {
		t.Fatalf("BeforeSeq=0 must be no-filter, got %d rows", len(got))
	}
	got = ApplyFilter(evts, Filter{AfterSeq: 1, BeforeSeq: 4})
	if len(got) != 2 || got[0].Seq != 2 || got[1].Seq != 3 {
		t.Fatalf("AfterSeq=1+BeforeSeq=4 -> %v, want seqs [2 3]", got)
	}
}
