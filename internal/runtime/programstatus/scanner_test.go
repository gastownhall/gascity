package programstatus

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

const (
	esc = "\x1b"
	bel = "\x07"
	st  = esc + `\`
)

// scanChunks feeds chunks to a fresh Scanner and returns every event.
func scanChunks(sc *Scanner, chunks ...[]byte) []Event {
	var got []Event
	for _, c := range chunks {
		sc.Feed(c, func(e Event) { got = append(got, e) })
	}
	return got
}

func scanString(chunks ...string) []Event {
	bs := make([][]byte, len(chunks))
	for i, c := range chunks {
		bs[i] = []byte(c)
	}
	return scanChunks(&Scanner{}, bs...)
}

func report(body string) Event        { return Event{Kind: EventReport, Body: body} }
func wrappedReport(body string) Event { return Event{Kind: EventReport, Body: body, Passthrough: true} }

var (
	probe       = Event{Kind: EventProbe}
	promptStart = Event{Kind: EventPromptStart}
)

func assertEvents(t *testing.T, got, want []Event) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events\n got %+v\nwant %+v", got, want)
	}
}

// capture is a golden pane-output capture: the bytes tmux's pipe-pane -O
// handed the experiment tap, and the size of every read it took.
type capture struct {
	data  []byte
	reads []int
}

func loadCapture(t *testing.T, name string) capture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name+".bin"))
	if err != nil {
		t.Fatalf("reading capture %s: %v", name, err)
	}
	raw, err := os.ReadFile(filepath.Join("testdata", name+".reads"))
	if err != nil {
		t.Fatalf("reading capture %s reads: %v", name, err)
	}
	var c capture
	c.data = data
	total := 0
	for _, line := range strings.Fields(string(raw)) {
		n, err := strconv.Atoi(line)
		if err != nil {
			t.Fatalf("capture %s reads: %v", name, err)
		}
		c.reads = append(c.reads, n)
		total += n
	}
	if total != len(data) {
		t.Fatalf("capture %s: reads sum to %d, data is %d bytes", name, total, len(data))
	}
	return c
}

// chunks returns the capture split exactly as tmux delivered it.
func (c capture) chunks() [][]byte {
	out := make([][]byte, 0, len(c.reads))
	off := 0
	for _, n := range c.reads {
		out = append(out, c.data[off:off+n])
		off += n
	}
	return out
}

func TestScannerReassemblesReportSplitBetweenEscAndBackslash(t *testing.T) {
	// Synthetic: the split falls exactly between ESC and '\' of ST.
	assertEvents(t, scanString(esc+"]7501;state=blocked:kind=permission"+esc, `\`+"tail"),
		[]Event{report("state=blocked:kind=permission")})

	// Golden: tmux 3.4 pipe-pane -O reads of a scripted emitter. One report
	// spans three reads with the boundary between ESC and '\', one spans a
	// mid-prefix split, and a 6 KiB report spans a 4095-byte read.
	c := loadCapture(t, "synthetic-split")
	chunks := c.chunks()
	splitAtST := false
	for i := 0; i+1 < len(chunks); i++ {
		if a, b := chunks[i], chunks[i+1]; len(a) > 0 && a[len(a)-1] == 0x1b && len(b) > 0 && b[0] == '\\' {
			splitAtST = true
		}
	}
	if !splitAtST {
		t.Fatal("fixture no longer splits a report between ESC and '\\'")
	}
	sc := &Scanner{}
	got := scanChunks(sc, chunks...)
	phase := []Event{
		probe,
		report("state=working:app=gen"),
		report("state=blocked:kind=permission:app=gen"),
		report("state=blocked:kind=question:msg=QWxsb3cgYHJtIC1yZiBidWlsZGA/:app=gen"),
		report("state=working:id=root/task-1:progress=40:app=gen"),
		// the 6 KiB done report is over the 4096-byte sequence limit: discarded
		promptStart,
		report("state=idle:app=gen"),
		report("state=clear"),
	}
	assertEvents(t, got, append(append([]Event{}, phase...), phase...))
	if sc.Discarded() != 2 {
		t.Errorf("Discarded() = %d, want 2 (one oversize report per phase)", sc.Discarded())
	}
	// Split invariance: the same bytes in one read give the same events.
	assertEvents(t, scanChunks(&Scanner{}, c.data), got)
}

func TestScannerReassemblesNearLimitReportAcrossReads(t *testing.T) {
	body := "state=done:msg=" + strings.Repeat("A", 2732) + ":zz=" + strings.Repeat("b", MaxSequence-len(oscPrefix7501)-2-len("state=done:msg=")-2732-len(":zz="))
	seq := []byte(esc + "]7501;" + body + st)
	if len(seq) != MaxSequence {
		t.Fatalf("sequence is %d bytes, want %d", len(seq), MaxSequence)
	}
	in := append([]byte("prefix output "), seq...)
	in = append(in, []byte(esc+"]7501;state=idle"+bel)...)
	sc := &Scanner{}
	got := scanChunks(sc, in[:4095], in[4095:])
	assertEvents(t, got, []Event{report(body), report("state=idle")})
	if sc.Discarded() != 0 {
		t.Errorf("Discarded() = %d", sc.Discarded())
	}
	// One byte over the limit is discarded, and the scanner recovers.
	over := []byte(esc + "]7501;" + body + "b" + st + esc + "]7501;state=idle" + bel)
	sc = &Scanner{}
	assertEvents(t, scanChunks(sc, over[:100], over[100:]), []Event{report("state=idle")})
	if sc.Discarded() != 1 {
		t.Errorf("Discarded() = %d, want 1", sc.Discarded())
	}
	// BEL is one byte shorter than ST: a BEL-terminated sequence of exactly
	// 4096 bytes fits.
	belBody := body + "b"
	assertEvents(t, scanString(esc+"]7501;"+belBody+bel), []Event{report(belBody)})
}

func TestScannerTerminatorsAndSplits(t *testing.T) {
	full := esc + "]7501;state=working:app=x" + bel + "text" + esc + "]7501;state=idle" + st
	want := []Event{report("state=working:app=x"), report("state=idle")}
	// Every two-way split gives the same events.
	for i := 0; i <= len(full); i++ {
		assertEvents(t, scanString(full[:i], full[i:]), want)
	}
	// Byte at a time.
	parts := make([]string, len(full))
	for i := range full {
		parts[i] = full[i : i+1]
	}
	assertEvents(t, scanString(parts...), want)
}

func TestScannerProbeAndPromptStart(t *testing.T) {
	assertEvents(t, scanString(esc+"]7501;?"+bel+esc+"[c"), []Event{probe})
	assertEvents(t, scanString(esc+"]7501;?"+st), []Event{probe})
	// Claude's captured probe context.
	assertEvents(t, scanString(esc+"[>0q"+esc+"[?u"+esc+"]7501;?"+bel+esc+"[c"+esc+"[?2026$p"+esc+"[c"), []Event{probe})
	// "?" plus anything is not a probe; it is a report body (ParseBody drops it).
	assertEvents(t, scanString(esc+"]7501;?x"+bel), []Event{report("?x")})
	// OSC 133;A, with or without parameters, is a prompt start.
	assertEvents(t, scanString(esc+"]133;A"+bel, esc+"]133;A;cl=m;aid=1"+st), []Event{promptStart, promptStart})
	// Other OSC 133 marks are not.
	assertEvents(t, scanString(esc+"]133;B"+bel+esc+"]133;D;0"+st+esc+"]133;AB"+bel), nil)
}

func TestScannerIgnoresOtherSequences(t *testing.T) {
	in := esc + "]0;title 7501;state=idle" + bel + // other OSC
		esc + "]75011;state=idle" + bel + // longer OSC number
		esc + "]10;?" + st + // color query
		esc + "[31m7501;state=idle" + bel + // CSI then plain text
		"]7501;state=idle" + bel + // no ESC
		esc + "P1$r0m" + st + // DCS that is not passthrough
		esc + "_Gpayload" + st // APC
	assertEvents(t, scanString(in), nil)
}

func TestScannerAbortsOnInterruptedSequence(t *testing.T) {
	// ESC followed by anything but '\' abandons the sequence and starts a
	// new escape, which may itself be a report.
	assertEvents(t, scanString(esc+"]7501;state=working"+esc+"]7501;state=idle"+bel), []Event{report("state=idle")})
	assertEvents(t, scanString(esc+"]7501;state=working"+esc+"[0m"+bel), nil)
	// CAN and SUB cancel a sequence.
	assertEvents(t, scanString(esc+"]7501;state=working\x18"+bel, esc+"]7501;state=done\x1a"+st), nil)
	// An escape inside a non-passthrough DCS does not start an OSC.
	assertEvents(t, scanString(esc+"Pq"+esc+"]7501;state=idle"+bel+st+esc+"]7501;state=done"+bel), []Event{report("state=done")})
}

func TestScannerUnwrapsTmuxPassthrough(t *testing.T) {
	wrap := func(inner string) string {
		return esc + "Ptmux;" + strings.ReplaceAll(inner, esc, esc+esc) + st
	}
	assertEvents(t, scanString(wrap(esc+"]7501;state=working:app=x"+bel)), []Event{wrappedReport("state=working:app=x")})
	assertEvents(t, scanString(wrap(esc+"]7501;state=idle"+st)), []Event{wrappedReport("state=idle")})
	assertEvents(t, scanString(wrap(esc+"]7501;?"+st)), []Event{{Kind: EventProbe, Passthrough: true}})
	// Split at every point, including inside the doubled ESC.
	full := "out" + wrap(esc+"]7501;state=blocked:kind=auth"+st) + esc + "]7501;state=done" + bel
	want := []Event{wrappedReport("state=blocked:kind=auth"), report("state=done")}
	for i := 0; i <= len(full); i++ {
		assertEvents(t, scanString(full[:i], full[i:]), want)
	}
	// An unterminated inner sequence does not leak past the passthrough's end.
	assertEvents(t, scanString(esc+"Ptmux;"+esc+esc+"]7501;state=working"+st+"state=idle"+bel), nil)
}

func TestScannerBoundsMemoryForUnterminatedSequences(t *testing.T) {
	sc := &Scanner{}
	big := []byte(esc + "]7501;state=working:zz=" + strings.Repeat("a", 1<<20))
	got := scanChunks(sc, big, []byte(bel+esc+"]7501;state=idle"+bel))
	assertEvents(t, got, []Event{report("state=idle")})
	if sc.Discarded() != 1 {
		t.Errorf("Discarded() = %d", sc.Discarded())
	}
	if c := cap(sc.top.buf); c > 2*MaxSequence {
		t.Errorf("buffer grew to %d bytes", c)
	}
	// Uninteresting OSCs are never buffered.
	sc = &Scanner{}
	scanChunks(sc, []byte(esc+"]0;"+strings.Repeat("t", 1<<20)+bel))
	if c := cap(sc.top.buf); c > 64 {
		t.Errorf("buffered an uninteresting OSC: cap %d", c)
	}
}
