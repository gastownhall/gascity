package programstatus

// EventKind is what a [Scanner] recognized.
type EventKind uint8

// The events a Scanner emits.
const (
	EventReport      EventKind = iota + 1 // an OSC 7501 report; Body holds its pairs
	EventProbe                            // the OSC 7501 support probe, ESC ] 7501 ; ?
	EventPromptStart                      // OSC 133;A, a shell prompt start
)

// Event is one complete sequence the Scanner recognized.
type Event struct {
	Kind EventKind
	// Body is the report body, the bytes between "ESC ] 7501 ;" and the
	// terminator. Empty for other kinds.
	Body string
	// Passthrough is set when the sequence arrived wrapped in a tmux DCS
	// passthrough (ESC P tmux; ... ESC \).
	Passthrough bool
}

const (
	esc7501   = "7501;"
	osc133A   = "133;A"
	tmuxDCS   = "tmux;"
	byteESC   = 0x1b
	byteBEL   = 0x07
	byteCAN   = 0x18
	byteSUB   = 0x1a
	stLen     = 2 // ESC \
	belLen    = 1
	oscIntro  = 2 // ESC ]
	maxBuffer = MaxSequence - oscIntro - belLen
)

// Scanner reassembles OSC 7501 reports and probes, and OSC 133;A prompt
// starts, from a terminal output stream fed in arbitrary reads. It accepts
// BEL and ST (ESC \) terminators and unwraps tmux DCS passthrough. An escape
// that interrupts a sequence abandons it and is itself scanned, as are CAN
// and SUB, which cancel it. A DCS that is not tmux passthrough is skipped to
// its ST, as tmux does. A 7501 sequence over [MaxSequence] bytes is
// discarded (counted by Discarded); no more than that is ever buffered.
// The zero value is ready to use. A Scanner is not safe for concurrent use.
type Scanner struct {
	top       machine
	inner     machine // tmux passthrough contents
	discarded uint64
}

// Feed scans p, calling emit for every sequence p completes.
func (s *Scanner) Feed(p []byte, emit func(Event)) {
	for _, c := range p {
		s.top.step(s, c, emit)
	}
}

// Discarded returns how many 7501 sequences were discarded for exceeding
// [MaxSequence].
func (s *Scanner) Discarded() uint64 { return s.discarded }

type scanState uint8

const (
	stGround   scanState = iota
	stEsc                // after ESC
	stOSC                // in an OSC string
	stOSCEsc             // in an OSC string, after ESC
	stDCSEntry           // in a DCS, matching "tmux;"
	stDCS                // in a DCS other than passthrough
	stDCSEsc             // in a DCS, after ESC
	stPass               // in tmux passthrough
	stPassEsc            // in tmux passthrough, after ESC
)

// oscKind classifies the OSC being read.
type oscKind uint8

const (
	oscPending oscKind = iota // not yet known
	osc7501                   // buffering a 7501 body
	oscPrompt                 // 133;A
	oscSkip                   // anything else, or a discarded 7501
)

// machine is one level of the scan: the stream itself, or the unwrapped
// contents of a tmux passthrough (nested machines skip every DCS).
type machine struct {
	state  scanState
	kind   oscKind
	buf    []byte // OSC bytes after "ESC ]", or DCS bytes while matching "tmux;"
	nested bool
}

func (m *machine) step(s *Scanner, c byte, emit func(Event)) {
	switch m.state {
	case stGround:
		if c == byteESC {
			m.state = stEsc
		}
	case stEsc:
		m.escape(c)
	case stOSC:
		switch c {
		case byteBEL:
			m.finishOSC(s, belLen, emit)
		case byteESC:
			m.state = stOSCEsc
		case byteCAN, byteSUB:
			m.state = stGround
		default:
			m.oscByte(s, c)
		}
	case stOSCEsc:
		if c == '\\' {
			m.finishOSC(s, stLen, emit)
			return
		}
		m.escape(c) // the interrupting escape starts a new sequence
	case stDCSEntry:
		if c == byteESC {
			m.state = stDCSEsc
			return
		}
		m.buf = append(m.buf, c)
		if string(m.buf) == tmuxDCS && !m.nested {
			m.state = stPass
			s.inner = machine{nested: true, buf: s.inner.buf[:0]}
		} else if len(m.buf) >= len(tmuxDCS) || m.nested || tmuxDCS[len(m.buf)-1] != c {
			m.state = stDCS
		}
	case stDCS:
		if c == byteESC {
			m.state = stDCSEsc
		}
	case stDCSEsc:
		if c == '\\' {
			m.state = stGround
		} else {
			m.state = stDCS
		}
	case stPass:
		if c == byteESC {
			m.state = stPassEsc
			return
		}
		s.inner.step(s, c, emit)
	case stPassEsc:
		if c == '\\' {
			m.state = stGround
			return
		}
		// ESC ESC carries an ESC; tmux keeps the byte after any other ESC.
		m.state = stPass
		s.inner.step(s, c, emit)
	}
}

// escape handles the byte after an ESC.
func (m *machine) escape(c byte) {
	switch c {
	case ']':
		m.state, m.kind, m.buf = stOSC, oscPending, m.buf[:0]
	case 'P':
		m.state, m.buf = stDCSEntry, m.buf[:0]
	case byteESC:
		m.state = stEsc
	default:
		m.state = stGround
	}
}

// oscByte accumulates one OSC body byte, classifying the OSC as soon as its
// prefix decides it.
func (m *machine) oscByte(s *Scanner, c byte) {
	switch m.kind {
	case oscSkip, oscPrompt:
		return
	case osc7501:
		if len(m.buf) >= maxBuffer {
			m.kind = oscSkip
			s.discarded++
			return
		}
		m.buf = append(m.buf, c)
		return
	}
	m.buf = append(m.buf, c)
	b := string(m.buf)
	switch {
	case b == esc7501:
		m.kind = osc7501
	case b == osc133A+";":
		m.kind = oscPrompt
	case len(b) <= len(esc7501) && esc7501[:len(b)] == b:
	case len(b) <= len(osc133A) && osc133A[:len(b)] == b:
	default:
		m.kind = oscSkip
	}
}

// finishOSC completes the OSC with a terminator of termLen bytes.
func (m *machine) finishOSC(s *Scanner, termLen int, emit func(Event)) {
	m.state = stGround
	switch {
	case m.kind == oscPrompt || (m.kind == oscPending && string(m.buf) == osc133A):
		emit(Event{Kind: EventPromptStart, Passthrough: m.nested})
	case m.kind == osc7501:
		if oscIntro+len(m.buf)+termLen > MaxSequence {
			s.discarded++
			return
		}
		body := m.buf[len(esc7501):]
		if string(body) == "?" {
			emit(Event{Kind: EventProbe, Passthrough: m.nested})
			return
		}
		emit(Event{Kind: EventReport, Body: string(body), Passthrough: m.nested})
	}
}
