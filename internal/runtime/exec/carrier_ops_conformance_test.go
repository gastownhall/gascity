package exec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimecontract/reference"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

func TestOpsCarrierIdleWaitContract(t *testing.T) {
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			fixture := func(screen string) func(*testing.T) (runtime.Provider, string) {
				return func(t *testing.T) (runtime.Provider, string) {
					pk := newOpsPack(t, opsIdleCaps)
					pk.put(t, "screen", screen)
					t.Cleanup(func() { pk.noTmux(t) })
					return ctor(pk.script), "s"
				}
			}
			runtimetest.RunIdleWaitContractTests(t, runtimetest.IdleWaitFixtures{Busy: fixture(busyClaudePane), Unobserved: fixture("no prompt"), Timeout: 200 * time.Millisecond})
		})
	}
}

// TestExecV2ConformanceNonTmux runs the provider contract over the ops golden
// and over its host-filesystem variant, whose handshake also declares fs.host.
// These starts carry no host work; TestExecHostStagingContractOpsCarrier
// covers that.
func TestExecV2ConformanceNonTmux(t *testing.T) {
	for name, bodies := range map[string]map[string]string{"base": reference.NonTmuxBodies(), "host-fs": reference.NonTmuxHostFSBodies()} {
		t.Run(name, func(t *testing.T) {
			state := t.TempDir()
			script := filepath.Join(t.TempDir(), "golden")
			if err := os.WriteFile(script, []byte(reference.Script(state, bodies)), 0o755); err != nil {
				t.Fatal(err)
			}
			sp := NewSeamBackedForCity(script, "ops-conformance", "/ops-conformance")
			if !runtime.FreshReadable(sp) || !runtime.ListRunningAttested(sp) {
				t.Fatal("non-tmux golden not v2-ready")
			}
			if raw := seamBacked(NewProviderForCity(script, "ops-conformance", "/ops-conformance")).raw; !raw.opsCarrierSelected() || raw.sharesHostFS() != (name == "host-fs") {
				t.Fatalf("golden handshake: carrier.ops selected = %v, shares the host filesystem = %v", raw.opsCarrierSelected(), raw.sharesHostFS())
			}
			var n atomic.Int64
			runtimetest.RunProviderTests(t, func(ct *testing.T) (runtime.Provider, runtime.Config, string) {
				return sp, runtime.Config{WorkDir: ct.TempDir(), Command: "sleep 300", ProcessNames: []string{"sleep"}, AcceptStartupDialogs: new(bool)}, fmt.Sprintf("ops-v2-%06d", n.Add(1))
			})
			if _, err := os.Stat(filepath.Join(state, "tmux-rejected")); !os.IsNotExist(err) {
				t.Error("production composition sent tmux to ops golden")
			}
		})
	}
}

// TestExecHostStagingContractOpsCarrier runs RUNTIME-CONTRACT-022 over the
// host-filesystem ops golden with every exec command logged: gc's host work
// around start (staging, pre_start, session_setup) sends no tmux under
// carrier.ops, also when the handshake declares the tokens that give the
// default carrier a separable launch and fenced input.
func TestExecHostStagingContractOpsCarrier(t *testing.T) {
	for name, extra := range map[string]string{"ops": "", "ops-with-default-carrier-tokens": `,"proc.exec","proc.provision","input-fence"`} {
		t.Run(name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "exec-log")
			runtimetest.RunHostStagingContract(t, runtimetest.HostStagingFixture{
				New: func(t *testing.T) (runtime.Provider, func(string) []string) {
					sp, received, _ := hostFSGoldenFixtureWith(t, func(b map[string]string) {
						for op, body := range reference.NonTmuxHostFSBodies() {
							b[op] = body
						}
						b["protocol"] = strings.Replace(b["protocol"], `"carrier.ops"`, `"carrier.ops"`+extra, 1)
						b["exec"] = `cmd=$(cat); printf '%s\n' "$cmd" >> '` + log + `'`
					})
					if !sp.raw.opsCarrierSelected() {
						t.Fatal("the host-filesystem ops golden did not select carrier.ops")
					}
					if extra != "" && !sp.raw.handshakeCapability(runtime.ProtocolCapabilityProvision) {
						t.Fatal("the handshake does not declare proc.provision")
					}
					return sp, received
				},
			})
			sent, err := os.ReadFile(log)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if strings.Contains(string(sent), "tmux") {
				t.Errorf("host work sent tmux under carrier.ops:\n%s", sent)
			}
		})
	}
}
