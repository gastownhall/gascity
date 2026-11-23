package programstatus

// probeBody is the probe after its ESC: "] 7 5 0 1 ; ?".
const probeBody = "]7501;?"

// ProbeMatcher detects the OSC 7501 support probe, ESC ] 7501 ; ? followed by
// BEL or ESC \, in a byte stream fed in arbitrary reads. It is the strict,
// allocation-free detector the probe-answering relay runs in the agent's
// data path. Like [Scanner], it skips DCS strings (tmux passthrough
// included) to their ST, and an escape interrupting a probe starts a new
// sequence; on every stream it counts exactly the probes a Scanner reports
// outside passthrough. The zero value is ready to use.
type ProbeMatcher struct {
	state probeState
	n     int // bytes of probeBody matched, in probeMatching
}

type probeState uint8

const (
	probeGround probeState = iota
	probeEsc
	probeMatching // matching probeBody
	probeTermEsc  // after the probe's '?', saw ESC
	probeDCS
	probeDCSEsc
)

// Feed scans p and returns how many probes p completes.
func (m *ProbeMatcher) Feed(p []byte) int {
	found := 0
	for _, c := range p {
		if m.step(c) {
			found++
		}
	}
	return found
}

func (m *ProbeMatcher) step(c byte) bool {
	switch m.state {
	case probeGround:
		if c == byteESC {
			m.state = probeEsc
		}
	case probeEsc:
		m.escape(c)
	case probeMatching:
		switch {
		case m.n < len(probeBody) && c == probeBody[m.n]:
			m.n++
		case m.n == len(probeBody) && c == byteBEL:
			m.state = probeGround
			return true
		case m.n == len(probeBody) && c == byteESC:
			m.state = probeTermEsc
		case c == byteESC:
			m.state = probeEsc
		default:
			m.state = probeGround
		}
	case probeTermEsc:
		if c == '\\' {
			m.state = probeGround
			return true
		}
		m.escape(c)
	case probeDCS:
		if c == byteESC {
			m.state = probeDCSEsc
		}
	case probeDCSEsc:
		if c == '\\' {
			m.state = probeGround
		} else {
			m.state = probeDCS
		}
	}
	return false
}

// escape handles the byte after an ESC.
func (m *ProbeMatcher) escape(c byte) {
	switch c {
	case ']':
		m.state, m.n = probeMatching, 1
	case 'P':
		m.state = probeDCS
	case byteESC:
		m.state = probeEsc
	default:
		m.state = probeGround
	}
}
