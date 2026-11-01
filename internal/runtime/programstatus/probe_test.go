package programstatus

import (
	"testing"
)

func TestProbeMatcherMatchesAcrossReads(t *testing.T) {
	for _, in := range []string{esc + "]7501;?" + bel, esc + "]7501;?" + st} {
		for i := 0; i <= len(in); i++ {
			var m ProbeMatcher
			n := m.Feed([]byte("pre" + in[:i]))
			n += m.Feed([]byte(in[i:] + esc + "[c"))
			if n != 1 {
				t.Errorf("split %q at %d: %d probes, want 1", in, i, n)
			}
		}
	}
}

func TestProbeMatcherReportsTheChunkThatCompletesTheProbe(t *testing.T) {
	var m ProbeMatcher
	if n := m.Feed([]byte(esc + "]7501;")); n != 0 {
		t.Fatalf("partial probe matched: %d", n)
	}
	if n := m.Feed([]byte("?" + esc)); n != 0 {
		t.Fatalf("probe before its terminator matched: %d", n)
	}
	if n := m.Feed([]byte(`\` + esc + "[c")); n != 1 {
		t.Fatalf("completing chunk: %d probes, want 1", n)
	}
}

func TestProbeMatcherIsStrict(t *testing.T) {
	for _, in := range []string{
		esc + "]7501;?x" + bel,
		esc + "]7501; ?" + bel,
		esc + "]7501;state=idle" + bel,
		esc + "]75010;?" + bel,
		esc + "]7501;?" + esc + "[c",
		"]7501;?" + bel,
		esc + "Pq" + esc + "]7501;?" + bel + st,           // inside a DCS
		esc + "Ptmux;" + esc + esc + "]7501;?" + bel + st, // inside tmux passthrough
	} {
		var m ProbeMatcher
		if n := m.Feed([]byte(in)); n != 0 {
			t.Errorf("%q: %d probes, want 0", in, n)
		}
	}
	var m ProbeMatcher
	if n := m.Feed([]byte(esc + "]7501;?" + bel + esc + esc + "]7501;?" + st + esc + "]0;x" + esc + "]7501;?" + bel)); n != 3 {
		t.Errorf("three probes: got %d", n)
	}
}

// probeCounts runs data through a Scanner and a ProbeMatcher, both fed in
// chunks of size chunk, and returns the Scanner's direct (non-passthrough)
// probe count and the matcher's.
func probeCounts(data []byte, chunk int) (scanner, matcher int) {
	var sc Scanner
	var m ProbeMatcher
	for off := 0; off < len(data); off += chunk {
		end := min(off+chunk, len(data))
		sc.Feed(data[off:end], func(e Event) {
			if e.Kind == EventProbe && !e.Passthrough {
				scanner++
			}
		})
		matcher += m.Feed(data[off:end])
	}
	return scanner, matcher
}

func FuzzProbeMatcherAgreesWithScanner(f *testing.F) {
	seeds := []string{
		esc + "]7501;?" + bel,
		esc + "]7501;?" + st,
		esc + "]7501;?" + esc + esc + `\`,
		esc + "]7501;??" + bel + esc + "]7501;?" + esc + "]7501;?" + bel,
		esc + "]0;" + esc + "]7501;?" + st,
		esc + "Pq" + esc + "]7501;?" + bel + st + esc + "]7501;?" + bel,
		esc + "Ptmux;" + esc + esc + "]7501;?" + esc + esc + `\` + st,
		esc + "Ptmux;" + esc + esc + "]7501;?" + bel + st + esc + "]7501;?" + st,
		esc + "]7501;state=working" + "\x18" + esc + "]7501;?" + bel,
		esc + "[>0q" + esc + "[?u" + esc + "]7501;?" + bel + esc + "[c",
		esc + esc + esc + "]7501;?" + bel,
	}
	for _, s := range seeds {
		f.Add([]byte(s), uint8(1))
		f.Add([]byte(s), uint8(3))
		f.Add([]byte(s), uint8(255))
	}
	f.Fuzz(func(t *testing.T, data []byte, chunk uint8) {
		size := int(chunk)%32 + 1
		s, m := probeCounts(data, size)
		if s != m {
			t.Fatalf("chunk %d: scanner saw %d probes, matcher %d, in %q", size, s, m, data)
		}
		// Both are split-invariant.
		whole, wholeM := probeCounts(data, len(data)+1)
		if whole != s || wholeM != m {
			t.Fatalf("chunk %d: split changed counts (%d,%d) vs whole (%d,%d) in %q", size, s, m, whole, wholeM, data)
		}
		// The Scanner's events are split-invariant too.
		var a, b []Event
		var sa, sb Scanner
		sa.Feed(data, func(e Event) { a = append(a, e) })
		for off := 0; off < len(data); off += size {
			sb.Feed(data[off:min(off+size, len(data))], func(e Event) { b = append(b, e) })
		}
		if len(a) != len(b) {
			t.Fatalf("chunk %d: %d events whole vs %d split in %q", size, len(a), len(b), data)
		}
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("chunk %d: event %d differs: %+v vs %+v", size, i, a[i], b[i])
			}
		}
	})
}
