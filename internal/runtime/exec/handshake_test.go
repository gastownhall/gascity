package exec

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// protocolScript returns a script body whose `protocol` op prints the
// given handshake output and counts invocations into counterFile. All
// other ops behave like allOpsScript, plus `is-attached` echoing true.
func protocolScript(handshake, counterFile string) string {
	return fmt.Sprintf(`
op="$1"

case "$op" in
  protocol)    echo run >> %q; printf '%%s' '%s' ;;
  is-attached) echo "true" ;;
  is-running)  echo "true" ;;
  start)       cat > /dev/null ;;
  stop)        ;;
  *) exit 2 ;;
esac
`, counterFile, handshake)
}

func newHandshakeProvider(t *testing.T, handshake string) (*Provider, string) {
	t.Helper()
	dir := t.TempDir()
	counterFile := filepath.Join(dir, "protocol-calls")
	script := writeScript(t, dir, protocolScript(handshake, counterFile))
	return NewProvider(script), counterFile
}

func protocolCallCount(t *testing.T, counterFile string) int {
	t.Helper()
	data, err := os.ReadFile(counterFile)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("reading protocol counter: %v", err)
	}
	return strings.Count(string(data), "run")
}

func TestProtocol_AbsentOpDefaultsToVersionZeroNoCapabilities(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript()) // no protocol op → exit 2
	p := NewProvider(script)

	info, err := p.Protocol()
	if err != nil {
		t.Fatalf("Protocol: %v", err)
	}
	if info.Version != 0 || len(info.Capabilities) != 0 {
		t.Fatalf("Protocol = %+v, want version 0 with no capabilities", info)
	}
	if caps := p.Capabilities(); caps.CanReportAttachment || caps.CanReportActivity {
		t.Fatalf("Capabilities = %+v, want zero capabilities", caps)
	}
	if p.IsAttached("sess") {
		t.Fatal("IsAttached = true without report-attachment capability, want false")
	}
}

func TestProtocol_DeclaredCapabilitiesEnableProbes(t *testing.T) {
	p, _ := newHandshakeProvider(t,
		`{"version":0,"capabilities":["report-attachment","report-activity"]}`)

	info, err := p.Protocol()
	if err != nil {
		t.Fatalf("Protocol: %v", err)
	}
	if info.Version != 0 {
		t.Fatalf("version = %d, want 0", info.Version)
	}
	caps := p.Capabilities()
	if !caps.CanReportAttachment || !caps.CanReportActivity {
		t.Fatalf("Capabilities = %+v, want both attachment and activity", caps)
	}
	if !p.IsAttached("sess") {
		t.Fatal("IsAttached = false, want true (script echoes true)")
	}
}

func TestProtocol_UnknownCapabilitiesIgnored(t *testing.T) {
	p, _ := newHandshakeProvider(t,
		`{"version":0,"capabilities":["future-shiny-cap","report-activity"]}`)

	caps := p.Capabilities()
	if caps.CanReportAttachment {
		t.Fatal("CanReportAttachment = true, want false (not declared)")
	}
	if !caps.CanReportActivity {
		t.Fatal("CanReportActivity = false, want true (declared)")
	}
}

func TestProtocol_MalformedJSONErrorsAndDegradesToZeroCapabilities(t *testing.T) {
	p, _ := newHandshakeProvider(t, `not json at all`)

	if _, err := p.Protocol(); err == nil {
		t.Fatal("Protocol with malformed handshake succeeded, want error")
	}
	if caps := p.Capabilities(); caps.CanReportAttachment || caps.CanReportActivity {
		t.Fatalf("Capabilities after malformed handshake = %+v, want zero floor", caps)
	}
	if p.IsAttached("sess") {
		t.Fatal("IsAttached after malformed handshake = true, want false")
	}
}

func TestProtocol_NegativeVersionIsHandshakeError(t *testing.T) {
	p, _ := newHandshakeProvider(t, `{"version":-1}`)

	if _, err := p.Protocol(); err == nil {
		t.Fatal("Protocol with negative version succeeded, want error")
	}
}

func TestProtocol_OpErrorPropagatesStderr(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
case "$1" in
  protocol) echo "handshake broke" >&2; exit 1 ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)
	_, err := p.Protocol()
	if err == nil {
		t.Fatal("Protocol with exit-1 op succeeded, want error")
	}
	if !strings.Contains(err.Error(), "handshake broke") {
		t.Fatalf("error %q does not carry script stderr", err)
	}
}

func TestProtocol_HandshakeRunsOncePerProvider(t *testing.T) {
	p, counterFile := newHandshakeProvider(t,
		`{"version":0,"capabilities":["report-attachment"]}`)

	if _, err := p.Protocol(); err != nil {
		t.Fatalf("Protocol: %v", err)
	}
	p.Capabilities()
	p.IsAttached("sess")
	p.Capabilities()
	if _, err := p.Protocol(); err != nil {
		t.Fatalf("Protocol (second): %v", err)
	}
	if n := protocolCallCount(t, counterFile); n != 1 {
		t.Fatalf("protocol op invoked %d times, want 1 (cached per provider)", n)
	}
}

func TestProtocol_IsAttachedErrorReadsAsFalse(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
case "$1" in
  protocol)    printf '{"version":0,"capabilities":["report-attachment"]}' ;;
  is-attached) echo "boom" >&2; exit 1 ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)
	if p.IsAttached("sess") {
		t.Fatal("IsAttached = true when op errors, want false")
	}
}

func TestProtocolInfoHas(t *testing.T) {
	info := runtime.ProtocolInfo{Capabilities: []string{"report-activity"}}
	if !info.Has(runtime.ProtocolCapabilityReportActivity) {
		t.Fatal("Has(report-activity) = false, want true")
	}
	if info.Has(runtime.ProtocolCapabilityReportAttachment) {
		t.Fatal("Has(report-attachment) = true, want false")
	}
}
