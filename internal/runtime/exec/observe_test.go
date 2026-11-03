package exec

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// readyHandshake declares the v2 profile: observe.v1 and report-attachment.
const readyHandshake = `{"version":0,"capabilities":["observe.v1","report-attachment"]}`

// secretToken stands for a GC_INSTANCE_TOKEN in a script's stdout; no error
// may carry it.
const secretToken = "tok-SECRET-0f0f0f"

// observePack writes a pack whose `protocol` op prints handshake (exit 2
// when handshake is empty), which logs every other op's name to dir/ops,
// and which runs ops[op] for it with $dir set; any other op exits 2.
func observePack(t *testing.T, handshake string, ops map[string]string) (script, dir string) {
	t.Helper()
	dir = t.TempDir()
	var b strings.Builder
	fmt.Fprintf(&b, "dir=%q\nop=\"$1\"; name=\"$2\"\n", dir)
	if handshake == "" {
		b.WriteString("[ \"$op\" = protocol ] && exit 2\n")
	} else {
		fmt.Fprintf(&b, "if [ \"$op\" = protocol ]; then printf '%%s' '%s'; exit 0; fi\n", handshake)
	}
	b.WriteString("echo \"$op\" >> \"$dir/ops\"\ncase \"$op\" in\n")
	names := make([]string, 0, len(ops))
	for op := range ops {
		names = append(names, op)
	}
	sort.Strings(names)
	for _, op := range names {
		fmt.Fprintf(&b, "  %s) %s ;;\n", op, ops[op])
	}
	b.WriteString("  *) exit 2 ;;\nesac\n")
	return writeScript(t, dir, b.String()), dir
}

// opsRan lists the ops the pack ran, in order.
func opsRan(t *testing.T, dir string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "ops"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const (
	testObject   = `"object":{"id":"c0ffee","created":"2026-10-30T01:02:03Z"}`
	testIdentity = `"identity":{"GC_SESSION_ID":"mc-1","GC_INSTANCE_TOKEN":"` + secretToken + `"}`
)

// Kills (§13.5 3, 4, 5, 14): an unknown answer read as absent, one mutant
// per row; identity null read as a known empty identity; an unknown or
// omitted agent read as dead; an answer's bytes echoed in its error.
func TestParseObserve(t *testing.T) {
	box := runtime.Liveness{ObjectID: "c0ffee", ObjectCreated: "2026-10-30T01:02:03Z"}
	running := func(alive bool, pid string) runtime.Liveness {
		l := box
		l.Running, l.Alive, l.PanePID = true, alive, pid
		return l
	}
	corpse := box
	corpse.Corpse = true
	names := []string{"claude", " "}
	for _, tc := range []struct {
		name     string
		in       string
		names    []string
		want     runtime.Liveness
		identity map[string]string
	}{
		{"absent", `{"state":"absent"}`, names, runtime.Liveness{}, nil},
		{"absent ignores the rest", `{"state":"absent","object":null,"agent":"bogus"}` + "\n", names, runtime.Liveness{}, nil},
		{"running, no names: agent ignored", `{"state":"running",` + testObject + `,"agent":"dead","agent_pid":"42",` + testIdentity + `}`, nil, running(true, "42"), map[string]string{"GC_SESSION_ID": "mc-1", "GC_INSTANCE_TOKEN": secretToken}},
		{"running, blank names only", `{"state":"running",` + testObject + `,"agent":"dead"}`, []string{" ", ""}, running(true, ""), nil},
		{"running, agent alive", `{"state":"running",` + testObject + `,"agent":"alive","identity":{}}`, names, running(true, ""), map[string]string{}},
		{"running, agent unknown", `{"state":"running",` + testObject + `,"agent":"unknown","identity":null}`, names, running(true, ""), nil},
		{"running, agent omitted", `{"state":"running",` + testObject + `}`, names, running(true, ""), nil},
		{"running, agent dead", `{"state":"running",` + testObject + `,"agent":"dead"}`, names, running(false, ""), nil},
		{"dead", `{"state":"dead",` + testObject + `,"agent":"dead","agent_pid":"7"}`, names, corpse, nil},
		{"dead, agent unknown", `{"state":"dead",` + testObject + `,"agent":"unknown"}`, nil, corpse, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, err := parseObserve([]byte(tc.in))
			if err != nil {
				t.Fatalf("parseObserve: %v", err)
			}
			if got := o.liveness(tc.names); got != tc.want {
				t.Errorf("liveness = %+v, want %+v", got, tc.want)
			}
			if (o.identity == nil) != (tc.identity == nil) || !reflect.DeepEqual(o.identity, tc.identity) && len(tc.identity) > 0 {
				t.Errorf("identity = %#v, want %#v", o.identity, tc.identity)
			}
		})
	}
	for _, tc := range []struct{ name, in string }{
		{"not JSON", `state=running ` + secretToken},
		{"empty", ``},
		{"null", `null`},
		{"array", `["` + secretToken + `"]`},
		{"second object", `{"state":"absent"}{"identity":"` + secretToken + `"}`},
		{"trailing bytes", `{"state":"absent"} ` + secretToken},
		{"state unknown", `{"state":"unknown","reason":"` + secretToken + `"}`},
		{"unrecognized state", `{"state":"zombie",` + testIdentity + `}`},
		{"state missing", `{` + testObject + `,` + testIdentity + `}`},
		{"running without object", `{"state":"running",` + testIdentity + `}`},
		{"running, empty object id", `{"state":"running","object":{"id":"","created":"t"},` + testIdentity + `}`},
		{"running, empty created", `{"state":"running","object":{"id":"c","created":" "},` + testIdentity + `}`},
		{"dead without object", `{"state":"dead",` + testIdentity + `}`},
		{"dead with a live agent", `{"state":"dead",` + testObject + `,"agent":"alive",` + testIdentity + `}`},
		{"unrecognized agent", `{"state":"running",` + testObject + `,"agent":"maybe",` + testIdentity + `}`},
		{"identity not an object", `{"state":"running",` + testObject + `,"identity":"` + secretToken + `"}`},
		{"identity value not a string", `{"state":"running",` + testObject + `,"identity":{"GC_RUNTIME_EPOCH":7,"GC_INSTANCE_TOKEN":"` + secretToken + `"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, err := parseObserve([]byte(tc.in))
			if err == nil {
				t.Fatalf("parseObserve = %+v, want an error (unknown, never absent)", o)
			}
			if strings.Contains(err.Error(), secretToken) || strings.Contains(err.Error(), "SECRET") {
				t.Errorf("error %q echoes the answer", err)
			}
		})
	}
}

// Kills (§13.5 3, 10, 14): a failed, slow, crashed or oversized observe
// read as absent; a probe bound of 5s or more; stdout echoed in an error.
// Through the production seam-backed provider.
func TestObserveLivenessThroughTheSeam(t *testing.T) {
	big := `printf '{"state":"absent","pad":"'; head -c 1100000 /dev/zero | tr '\0' a; printf '"}'`
	for _, tc := range []struct {
		name    string
		observe string
		want    runtime.Liveness
		wantErr bool
	}{
		{"absent", `cat > "$dir/stdin"; echo '{"state":"absent"}'`, runtime.Liveness{}, false},
		{"running", `cat > "$dir/stdin"; echo '{"state":"running",` + testObject + `,"agent":"dead","agent_pid":"9"}'`, runtime.Liveness{Running: true, ObjectID: "c0ffee", ObjectCreated: "2026-10-30T01:02:03Z", PanePID: "9"}, false},
		{"dead", `cat > "$dir/stdin"; echo '{"state":"dead",` + testObject + `}'`, runtime.Liveness{Corpse: true, ObjectID: "c0ffee", ObjectCreated: "2026-10-30T01:02:03Z"}, false},
		{"unknown", `echo '{"state":"unknown"}'`, runtime.Liveness{}, true},
		{"exit 1", `echo '{"state":"absent"}'; exit 1`, runtime.Liveness{}, true},
		{"exit 2", `echo '{"state":"absent"}'; exit 2`, runtime.Liveness{}, true},
		{"crash", `echo '{"state":"absent"}'; kill -9 $$`, runtime.Liveness{}, true},
		{"oversized", big, runtime.Liveness{}, true},
		{"malformed with a token", `echo '{"state":"running","identity":{"GC_INSTANCE_TOKEN":"` + secretToken + `"}}'`, runtime.Liveness{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script, dir := observePack(t, readyHandshake, map[string]string{"observe": tc.observe})
			sp := NewSeamBacked(script)
			got, err := sp.(runtime.LivenessObserverWithError).ObserveLivenessWithError("s", []string{"claude", "node"})
			if tc.wantErr {
				if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
					t.Fatalf("err = %v, want ErrRuntimeUnavailable", err)
				}
				if strings.Contains(err.Error(), secretToken) || strings.Contains(err.Error(), "aaaa") {
					t.Errorf("error %q echoes stdout", err)
				}
			} else if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got != tc.want {
				t.Errorf("liveness = %+v, want %+v", got, tc.want)
			}
			if !tc.wantErr {
				if stdin := readLog(t, filepath.Join(dir, "stdin")); stdin != "claude\nnode\n" {
					t.Errorf("observe stdin = %q, want the process names one per line", stdin)
				}
			}
		})
	}
}

func TestObserveSlowProbeIsCanceledAt4s(t *testing.T) {
	script, dir := observePack(t, readyHandshake, map[string]string{
		"observe": `trap 'echo term >> "$dir/term"; exit 143' TERM; sleep 6 & wait $!; echo '{"state":"absent"}'`,
	})
	p := NewProvider(script)
	if _, err := p.V2Readiness(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := p.ObserveLivenessWithError("s", nil)
	elapsed := time.Since(start)
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("err = %v, want ErrRuntimeUnavailable", err)
	}
	if elapsed < 3500*time.Millisecond || elapsed >= 5*time.Second {
		t.Errorf("observe returned after %v, want about 4s and under v2's 5s fence", elapsed)
	}
	if readLog(t, filepath.Join(dir, "term")) == "" {
		t.Error("the script never saw SIGTERM")
	}
}

// Kills (§13.5 9, 10, D8): a lax is-attached parse, or a v2-ready probe
// bound of 5s or more.
func TestIsAttachedStrictAndBoundedWhenReady(t *testing.T) {
	for _, out := range []string{"yes", "True", "", "true false", "1"} {
		script, _ := observePack(t, `{"version":0,"capabilities":["report-attachment"]}`, map[string]string{"is-attached": fmt.Sprintf("printf '%%s' %q", out)})
		if attached, err := NewProvider(script).IsAttachedWithError("s"); attached || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Errorf("is-attached %q: (%v, %v), want ErrRuntimeUnavailable", out, attached, err)
		}
	}
	script, _ := observePack(t, readyHandshake, map[string]string{"is-attached": `sleep 6 & wait $!; echo false`})
	p := NewProvider(script)
	if _, err := p.V2Readiness(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := p.IsAttachedWithError("s"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("slow is-attached: err = %v, want ErrRuntimeUnavailable", err)
	}
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Errorf("slow is-attached returned after %v, want under 5s", elapsed)
	}
}

// legacyOps answers like a v0 pack: is-running, process-alive, and exit 2
// (unknown op) for list-running, stop and the metadata ops.
var legacyOps = map[string]string{
	"is-running":    `echo true`,
	"process-alive": `cat >/dev/null; echo false`,
	"list-running":  `exit 2`,
	"stop":          `exit 2`,
	"get-meta":      `exit 2`,
	"set-meta":      `cat >/dev/null; exit 2`,
	"remove-meta":   `exit 2`,
	"observe":       `echo '{"state":"absent"}'`,
	"kill-object":   `cat >/dev/null; echo killed`,
	"is-attached":   `echo false`,
	"start":         `cat > "$dir/start.stdin"`,
}

// Kills (§13.5 1, 2, 3): a gate opened by a script that does not declare
// the v2 profile, or by a handshake error, or by observe.v1 without
// report-attachment. Such scripts keep the legacy behavior exactly: the
// error-bearing read is today's IsRunning/ProcessAlive read, exit 2 still
// reads as an empty list, a stop and an unset key, and no observe.v1 op
// ever runs.
func TestNotReadyScriptsKeepLegacyBehavior(t *testing.T) {
	for _, tc := range []struct {
		name, handshake string
		missing         []string
		handshakeErr    bool
	}{
		{"no handshake", "", []string{runtime.ProtocolCapabilityObserveV1, runtime.ProtocolCapabilityReportAttachment}, false},
		{"report-attachment only", `{"version":0,"capabilities":["report-attachment"]}`, []string{runtime.ProtocolCapabilityObserveV1}, false},
		{"observe.v1 without report-attachment", `{"version":0,"capabilities":["observe.v1"]}`, []string{runtime.ProtocolCapabilityReportAttachment}, false},
		{"broken handshake", `{"version":-1,"capabilities":["observe.v1","report-attachment"]}`, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script, dir := observePack(t, tc.handshake, legacyOps)
			sp := NewSeamBacked(script)
			missing, err := sp.(runtime.V2ReadinessReporter).V2Readiness()
			if tc.handshakeErr != (err != nil) || !slices.Equal(missing, tc.missing) {
				t.Fatalf("V2Readiness = %v, %v; want %v (handshake error %v)", missing, err, tc.missing, tc.handshakeErr)
			}
			if runtime.FreshReadable(sp) {
				t.Error("FreshReadable = true")
			}
			if sp.(runtime.IdentityObserver).IdentityObservable() {
				t.Error("IdentityObservable = true")
			}
			pn := []string{"claude"}
			got, err := sp.(runtime.LivenessObserverWithError).ObserveLivenessWithError("s", pn)
			if want := runtime.ObserveLiveness(struct{ runtime.Provider }{sp}, "s", pn); err != nil || got != want {
				t.Errorf("ObserveLivenessWithError = %+v, %v; want the legacy read %+v, nil", got, err, want)
			}
			if names, err := sp.ListRunning("s"); err != nil || names == nil || len(names) != 0 {
				t.Errorf("ListRunning = %#v, %v; want [], nil", names, err)
			}
			if err := sp.Stop("s"); err != nil {
				t.Errorf("Stop = %v, want nil", err)
			}
			if v, err := sp.GetMeta("s", "k"); v != "" || err != nil {
				t.Errorf("GetMeta = %q, %v; want \"\", nil", v, err)
			}
			if err := sp.SetMeta("s", "k", "v"); err != nil {
				t.Errorf("SetMeta = %v", err)
			}
			if err := sp.RemoveMeta("s", "k"); err != nil {
				t.Errorf("RemoveMeta = %v", err)
			}
			if !sp.IsRunning("s") {
				t.Error("IsRunning = false, want is-running's answer")
			}
			if _, err := sp.(runtime.SessionObjectKiller).KillCorpseObject("s", "c0ffee", "t"); !errors.Is(err, runtime.ErrSessionObjectKillUnsupported) {
				t.Errorf("KillCorpseObject = %v, want ErrSessionObjectKillUnsupported", err)
			}
			if _, err := sp.(runtime.IdentityObserver).ObserveIdentity("s"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Errorf("ObserveIdentity = %v, want ErrRuntimeUnavailable", err)
			}
			if err := sp.Start(t.Context(), "s", runtime.Config{}); err != nil {
				t.Fatalf("Start = %v", err)
			}
			if stdin := readLog(t, filepath.Join(dir, "start.stdin")); strings.Contains(stdin, "fresh_only") {
				t.Errorf("legacy start stdin %q carries fresh_only", stdin)
			}
			if ran := opsRan(t, dir); slices.Contains(ran, "observe") || slices.Contains(ran, "kill-object") {
				t.Errorf("ops %v: an observe.v1 op ran for a script that is not v2-ready", ran)
			}
		})
	}
}

// Kills (§13.5 2, 12): an interface the production seam does not forward,
// or a fresh read claimed by a nil provider.
func TestReadySeamIsFreshReadable(t *testing.T) {
	script, _ := observePack(t, readyHandshake, nil)
	sp := NewSeamBacked(script)
	if !runtime.FreshReadable(sp) {
		t.Error("FreshReadable(NewSeamBacked(ready)) = false")
	}
	if !sp.(runtime.IdentityObserver).IdentityObservable() {
		t.Error("IdentityObservable = false")
	}
	if missing, err := sp.(runtime.V2ReadinessReporter).V2Readiness(); missing != nil || err != nil {
		t.Errorf("V2Readiness = %v, %v; want ready", missing, err)
	}
	if (*Provider)(nil).LivenessReadsFresh() || (*seamBackedProvider)(nil).LivenessReadsFresh() {
		t.Error("a nil provider reads fresh")
	}
	if (*Provider)(nil).IdentityObservable() || (*seamBackedProvider)(nil).IdentityObservable() {
		t.Error("a nil provider observes identity")
	}
}

// Kills (§13.2, D7): exit 2 from an op a v2-ready script declared read as
// the unknown-op no-op: an empty listing, a stop that stopped nothing, an
// unset key.
func TestReadyScriptExitTwoIsAFailure(t *testing.T) {
	script, _ := observePack(t, readyHandshake, legacyOps)
	sp := NewSeamBacked(script)
	if names, err := sp.ListRunning("s"); err == nil || runtime.IsPartialListError(err) || names != nil {
		t.Errorf("ListRunning = %v, %v; want the inventory's exit 2 as a failure", names, err)
	}
	if err := sp.Stop("s"); !errors.Is(err, runtime.ErrStopUnsupported) {
		t.Errorf("Stop = %v, want ErrStopUnsupported", err)
	}
	if _, err := sp.GetMeta("s", "k"); !errors.Is(err, runtime.ErrMetaUnsupported) {
		t.Errorf("GetMeta = %v, want ErrMetaUnsupported", err)
	}
	if err := sp.SetMeta("s", "k", "v"); !errors.Is(err, runtime.ErrMetaUnsupported) {
		t.Errorf("SetMeta = %v, want ErrMetaUnsupported", err)
	}
	if err := sp.RemoveMeta("s", "k"); !errors.Is(err, runtime.ErrMetaUnsupported) {
		t.Errorf("RemoveMeta = %v, want ErrMetaUnsupported", err)
	}
	strict, _ := observePack(t, readyHandshake, map[string]string{"is-running": `echo true; exit 2`, "observe": `exit 2`, "kill-object": `cat >/dev/null; echo boom >&2; exit 2`})
	p := NewProvider(strict)
	if p.IsRunning("s") {
		t.Error("IsRunning after exit 2 = true")
	}
	if _, definite := p.isRunningOp("s"); definite {
		t.Error("is-running exit 2 is an answer")
	}
	if _, err := p.ObserveLivenessWithError("s", nil); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Errorf("observe exit 2: %v, want ErrRuntimeUnavailable", err)
	}
	if res, err := p.KillCorpseObject("s", "c0ffee", "t"); err == nil || res != runtime.SessionObjectNotKilled || !strings.Contains(err.Error(), "boom") {
		t.Errorf("kill-object exit 2: %v, %v; want not killed with the op's failure", res, err)
	}
}

// A legacy (not FreshOnly) start on a v2-ready pack asks `observe` whether
// a box held the name before it: a dead box is occupied, so a failed start
// leaves it, where is-running alone would read it vacant and tear it down.
// Kills: boxOccupancy ignoring observe on a v2-ready pack.
func TestReadyStartReadsOccupancyFromObserve(t *testing.T) {
	script, dir := observePack(t, readyHandshake, map[string]string{
		"observe":    `cat >/dev/null; echo '{"state":"dead",` + testObject + `}'`,
		"is-running": `echo false`,
		"start":      `cat >/dev/null; echo broken >&2; exit 1`,
		"stop":       `:`,
	})
	if err := NewSeamBacked(script).Start(t.Context(), "s", runtime.Config{}); err == nil {
		t.Fatal("Start = nil, want the start failure")
	}
	if ran := opsRan(t, dir); slices.Contains(ran, "stop") || !slices.Contains(ran, "observe") {
		t.Errorf("ops %v: want occupancy from observe and the dead box left", ran)
	}
}

func TestObserveIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, observe string
		want          map[string]string
		wantErr       error
	}{
		{"identity", `echo '{"state":"running",` + testObject + `,"identity":{"GC_SESSION_ID":"mc-1","GC_RUNTIME_EPOCH":"3"}}'`, map[string]string{"GC_SESSION_ID": "mc-1", "GC_RUNTIME_EPOCH": "3"}, nil},
		{"no gc identity", `echo '{"state":"dead",` + testObject + `,"identity":{}}'`, map[string]string{}, nil},
		{"not yet readable", `echo '{"state":"running",` + testObject + `,"identity":null}'`, nil, runtime.ErrRuntimeUnavailable},
		{"identity omitted", `echo '{"state":"running",` + testObject + `}'`, nil, runtime.ErrRuntimeUnavailable},
		{"absent", `echo '{"state":"absent"}'`, nil, runtime.ErrSessionNotFound},
		{"unknown", `echo '{"state":"unknown"}'`, nil, runtime.ErrRuntimeUnavailable},
		{"failure", `exit 1`, nil, runtime.ErrRuntimeUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script, dir := observePack(t, readyHandshake, map[string]string{"observe": `cat > "$dir/stdin"; ` + tc.observe})
			got, err := NewSeamBacked(script).(runtime.IdentityObserver).ObserveIdentity("s")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || got != nil {
					t.Fatalf("ObserveIdentity = %v, %v; want %v", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got == nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ObserveIdentity = %#v, %v; want %#v", got, err, tc.want)
			}
			if stdin := readLog(t, filepath.Join(dir, "stdin")); stdin != "" {
				t.Errorf("identity observe stdin = %q, want no process names", stdin)
			}
		})
	}
}

func TestKillObject(t *testing.T) {
	for out, want := range map[string]runtime.SessionObjectKillResult{
		"killed": runtime.SessionObjectKilled, "gone": runtime.SessionObjectGone, "renamed": runtime.SessionObjectRenamed,
		"live": runtime.SessionObjectLive, "changed": runtime.SessionObjectChanged,
	} {
		script, _ := observePack(t, readyHandshake, map[string]string{"kill-object": `cat >/dev/null; echo ` + out})
		if got, err := NewSeamBacked(script).(runtime.SessionObjectKiller).KillCorpseObject("s", "c0ffee", "t0"); got != want || err != nil {
			t.Errorf("kill-object %q = %v, %v; want %v", out, got, err, want)
		}
	}

	script, dir := observePack(t, readyHandshake, map[string]string{"kill-object": `cat > "$dir/stdin"; echo "$2" > "$dir/name"; echo killed`})
	killer := NewSeamBacked(script).(runtime.SessionObjectKiller)
	if _, err := killer.KillZombieObject("s", "c0ffee", "t0", "4242"); err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	if err := json.Unmarshal([]byte(readLog(t, filepath.Join(dir, "stdin"))), &req); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"object": map[string]any{"id": "c0ffee", "created": "t0"}, "mode": "zombie", "agent_pid": "4242"}
	if !reflect.DeepEqual(req, want) || readLog(t, filepath.Join(dir, "name")) != "s\n" {
		t.Errorf("kill-object got %v for %q, want %v for s", req, readLog(t, filepath.Join(dir, "name")), want)
	}
	if _, err := killer.KillZombieObject("s", "c0ffee", "t0", ""); err != nil {
		t.Errorf("zombie kill with no agent pid: %v, want allowed", err)
	}
	if err := json.Unmarshal([]byte(readLog(t, filepath.Join(dir, "stdin"))), &req); err != nil || req["mode"] != "zombie" {
		t.Errorf("kill-object stdin %v, %v", req, err)
	}

	before := len(opsRan(t, dir))
	for _, c := range [][3]string{{"", "c0ffee", "t0"}, {"s", "", "t0"}, {"s", "c0ffee", ""}, {"s", " ", "t0"}} {
		if res, err := killer.KillCorpseObject(c[0], c[1], c[2]); res != runtime.SessionObjectNotKilled || !errors.Is(err, runtime.ErrInvalidSessionObject) {
			t.Errorf("KillCorpseObject%q = %v, %v; want ErrInvalidSessionObject", c, res, err)
		}
	}
	if after := len(opsRan(t, dir)); after != before {
		t.Errorf("an invalid kill request ran an op")
	}

	for name, body := range map[string]string{"unrecognized": `cat >/dev/null; echo ` + secretToken, "failure": `cat >/dev/null; exit 1`} {
		script, _ := observePack(t, readyHandshake, map[string]string{"kill-object": body})
		res, err := NewProvider(script).KillCorpseObject("s", "c0ffee", "t0")
		if res != runtime.SessionObjectNotKilled || err == nil || strings.Contains(err.Error(), secretToken) {
			t.Errorf("%s: %v, %v; want not killed with an error that echoes nothing", name, res, err)
		}
	}
}

// freshOps is a stateful welded pack: `observe` prints dir/state, or
// absent without it; `start` saves its stdin, copies dir/after_start to
// dir/state, writes dir/start_stderr to stderr and exits dir/start_exit;
// `stop` removes dir/state.
var freshOps = map[string]string{
	"observe": `cat >/dev/null; if [ -f "$dir/state" ]; then cat "$dir/state"; else echo '{"state":"absent"}'; fi`,
	"start": `cat > "$dir/start.stdin"; [ -f "$dir/after_start" ] && cp "$dir/after_start" "$dir/state"; ` +
		`[ -f "$dir/start_stderr" ] && cat "$dir/start_stderr" >&2; code=0; [ -f "$dir/start_exit" ] && code=$(cat "$dir/start_exit"); exit $code`,
	"stop": `rm -f "$dir/state"`,
}

func boxWithToken(state, token string) string {
	if token == "null" {
		return `{"state":"` + state + `",` + testObject + `,"identity":null}`
	}
	return `{"state":"` + state + `",` + testObject + `,"identity":{"GC_INSTANCE_TOKEN":"` + token + `"}}`
}

// Kills (§13.5 7, F2): the FreshOnly pre-check skipped, unknown read as
// vacant, a cleanup without the token check, fresh_only not sent, and a
// collision cleaned up.
func TestFreshOnlyStart(t *testing.T) {
	const ours = "tok-ours"
	for _, tc := range []struct {
		name       string
		handshake  string
		raw        bool
		state      string
		afterStart string
		exit       string
		stderr     string
		wantErr    error
		wantOps    []string
	}{
		{name: "vacant", wantOps: []string{"observe", "start"}},
		{name: "vacant, raw provider", raw: true, wantOps: []string{"observe", "start"}},
		{name: "held live", state: boxWithToken("running", "tok-other"), wantErr: runtime.ErrSessionExists, wantOps: []string{"observe"}},
		{name: "held dead", state: boxWithToken("dead", ours), wantErr: runtime.ErrSessionExists, wantOps: []string{"observe"}},
		{name: "pre-check unknown", state: `{"state":"unknown"}`, wantErr: runtime.ErrRuntimeUnavailable, wantOps: []string{"observe"}},
		{name: "pre-check malformed", state: `{"state":"running"}`, wantErr: runtime.ErrRuntimeUnavailable, wantOps: []string{"observe"}},
		{name: "not v2-ready", handshake: `{"version":0,"capabilities":["report-attachment"]}`, wantErr: runtime.ErrRuntimeUnavailable},
		{name: "collision exit 73", afterStart: boxWithToken("running", ours), exit: "73", wantErr: runtime.ErrSessionExists, wantOps: []string{"observe", "start"}},
		{name: "collision phrase", afterStart: boxWithToken("running", ours), exit: "1", stderr: "box already running", wantErr: runtime.ErrSessionExists, wantOps: []string{"observe", "start"}},
		{name: "failure, our box", afterStart: boxWithToken("running", ours), exit: "1", wantOps: []string{"observe", "start", "observe", "stop"}},
		{name: "failure, our dead box", afterStart: boxWithToken("dead", ours), exit: "1", wantOps: []string{"observe", "start", "observe", "stop"}},
		{name: "failure, foreign box", afterStart: boxWithToken("running", "tok-other"), exit: "1", wantOps: []string{"observe", "start", "observe"}},
		{name: "failure, identity unreadable", afterStart: boxWithToken("running", "null"), exit: "1", wantOps: []string{"observe", "start", "observe"}},
		{name: "failure, no box", exit: "1", wantOps: []string{"observe", "start", "observe"}},
		{name: "failure, box unknown", afterStart: `{"state":"unknown"}`, exit: "1", wantOps: []string{"observe", "start", "observe"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handshake := tc.handshake
			if handshake == "" {
				handshake = readyHandshake
			}
			script, dir := observePack(t, handshake, freshOps)
			for file, content := range map[string]string{"state": tc.state, "after_start": tc.afterStart, "start_exit": tc.exit, "start_stderr": tc.stderr} {
				if content != "" {
					writeFile(t, filepath.Join(dir, file), content)
				}
			}
			sp := NewSeamBacked(script)
			if tc.raw {
				sp = NewProvider(script)
			}
			cfg := runtime.Config{FreshOnly: true, Env: map[string]string{"GC_INSTANCE_TOKEN": ours}, ProcessNames: []string{"claude"}, AcceptStartupDialogs: new(bool)}
			err := sp.Start(t.Context(), "s", cfg)
			switch {
			case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
				t.Errorf("Start = %v, want %v", err, tc.wantErr)
			case tc.wantErr == nil && tc.exit == "" && err != nil:
				t.Errorf("Start = %v, want nil", err)
			case tc.wantErr == nil && tc.exit != "" && (err == nil || errors.Is(err, runtime.ErrSessionExists)):
				t.Errorf("Start = %v, want the start failure", err)
			}
			if got := opsRan(t, dir); !slices.Equal(got, tc.wantOps) {
				t.Errorf("ops = %v, want %v", got, tc.wantOps)
			}
			if !slices.Contains(tc.wantOps, "start") {
				return
			}
			var sent struct {
				FreshOnly bool              `json:"fresh_only"`
				Env       map[string]string `json:"env"`
			}
			if err := json.Unmarshal([]byte(readLog(t, filepath.Join(dir, "start.stdin"))), &sent); err != nil {
				t.Fatal(err)
			}
			if !sent.FreshOnly || sent.Env["GC_INSTANCE_TOKEN"] != ours || sent.Env["GT_PROCESS_NAMES"] != "claude" {
				t.Errorf("start config fresh_only=%v env=%v; want fresh_only, our token and GT_PROCESS_NAMES", sent.FreshOnly, sent.Env)
			}
		})
	}
}

// A fresh start with no GC_INSTANCE_TOKEN mints one per start, aligned
// with the holder token, and leaves a configured GT_PROCESS_NAMES alone.
func TestFreshOnlyStartMintsToken(t *testing.T) {
	script, dir := observePack(t, readyHandshake, freshOps)
	sp := NewSeamBacked(script)
	var tokens []string
	for range 2 {
		if err := sp.Start(t.Context(), "s", runtime.Config{FreshOnly: true, ProcessNames: []string{"claude"}, AcceptStartupDialogs: new(bool), Env: map[string]string{"GT_PROCESS_NAMES": "codex"}}); err != nil {
			t.Fatal(err)
		}
		var sent struct {
			Env map[string]string `json:"env"`
		}
		if err := json.Unmarshal([]byte(readLog(t, filepath.Join(dir, "start.stdin"))), &sent); err != nil {
			t.Fatal(err)
		}
		tok := sent.Env["GC_INSTANCE_TOKEN"]
		if len(tok) != 32 || sent.Env["BEADS_HOLDER_TOKEN"] != tok || sent.Env["GT_PROCESS_NAMES"] != "codex" {
			t.Fatalf("env %v: want a minted token, the holder token aligned and GT_PROCESS_NAMES kept", sent.Env)
		}
		tokens = append(tokens, tok)
	}
	if tokens[0] == tokens[1] {
		t.Error("two fresh starts minted one token")
	}
}

// Kills (F2, S3): a fresh launch that respawns into an existing agent
// session, or that reads its own refusal as a collision and so leaks the
// box this attempt provisioned. Only `start`/`provision`'s exit 73 or
// collision phrase is a collision; a refused launch is a start failure, and
// the box carrying this attempt's token is stopped.
func TestFreshOnlySeparableLaunchNeverRespawns(t *testing.T) {
	const ours = "tok-ours"
	for _, tc := range []struct {
		name       string
		hasSession string
		provisions string // what provision leaves under the name
		wantErr    bool
		wantStop   bool
		wantLaunch string
	}{
		{name: "warm box refused, ours stopped", hasSession: "0", provisions: boxWithToken("running", ours), wantErr: true, wantStop: true},
		{name: "warm box refused, foreign kept", hasSession: "0", provisions: boxWithToken("running", "tok-other"), wantErr: true},
		{name: "cold box launched", hasSession: "1", provisions: boxWithToken("running", ours), wantLaunch: "new-session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script, dir := observePack(t, `{"version":0,"capabilities":["proc.exec","proc.provision","observe.v1","report-attachment"]}`, map[string]string{
				"observe":   freshOps["observe"],
				"stop":      freshOps["stop"],
				"provision": `cat > "$dir/provision.stdin"; cp "$dir/after_provision" "$dir/state"`,
				"exec":      `cmd="$(cat)"; echo "$cmd" >> "$dir/exec.log"; case "$cmd" in *has-session*) exit ` + tc.hasSession + ` ;; *) exit 0 ;; esac`,
			})
			writeFile(t, filepath.Join(dir, "after_provision"), tc.provisions)
			err := NewSeamBacked(script).Start(t.Context(), "s", runtime.Config{FreshOnly: true, Command: "claude", Env: map[string]string{"GC_INSTANCE_TOKEN": ours}})
			if tc.wantErr != (err != nil) || errors.Is(err, runtime.ErrSessionExists) {
				t.Fatalf("Start = %v, want a start failure %v (never ErrSessionExists)", err, tc.wantErr)
			}
			execLog := readLog(t, filepath.Join(dir, "exec.log"))
			if strings.Contains(execLog, "respawn-pane") {
				t.Errorf("exec log %q: a fresh launch respawned", execLog)
			}
			if tc.wantLaunch != "" && !strings.Contains(execLog, tc.wantLaunch) {
				t.Errorf("exec log %q, want %s", execLog, tc.wantLaunch)
			}
			if !strings.Contains(readLog(t, filepath.Join(dir, "provision.stdin")), `"fresh_only":true`) {
				t.Error("provision did not carry fresh_only")
			}
			ran := opsRan(t, dir)
			if slices.Contains(ran, "stop") != tc.wantStop || slices.Contains(ran, "start") {
				t.Errorf("ops %v: want stop %v and no welded start", ran, tc.wantStop)
			}
		})
	}
}

// Kills (S2): a fresh welded start that skips the post-start steps Start
// runs: on an input-fence pack the new incarnation's fence is published
// after the start op, and a publish that fails is a start failure whose
// cleanup stops the box carrying this attempt's token.
func TestFreshOnlyStartPublishesTheFence(t *testing.T) {
	const ours = "tok-ours"
	for _, failPublish := range []bool{false, true} {
		t.Run(fmt.Sprintf("publish fails %v", failPublish), func(t *testing.T) {
			publishExit := "0"
			if failPublish {
				publishExit = "1"
			}
			script, dir := observePack(t, `{"version":0,"capabilities":["proc.exec","input-fence","observe.v1","report-attachment"]}`, map[string]string{
				"observe": freshOps["observe"],
				"start":   freshOps["start"],
				"stop":    freshOps["stop"],
				"exec":    `cmd="$(cat)"; echo "$cmd" >> "$dir/exec.log"; case "$cmd" in *set-option*-F*) exit ` + publishExit + ` ;; esac`,
			})
			writeFile(t, filepath.Join(dir, "after_start"), boxWithToken("running", ours))
			cfg := runtime.Config{FreshOnly: true, AcceptStartupDialogs: new(bool), Env: map[string]string{runtime.InstanceTokenEnv: ours, runtime.RuntimeEpochEnv: "2"}}
			err := NewSeamBacked(script).Start(t.Context(), "s", cfg)
			if failPublish != (err != nil) || errors.Is(err, runtime.ErrSessionExists) {
				t.Fatalf("Start = %v, want failure %v", err, failPublish)
			}
			if execLog := readLog(t, filepath.Join(dir, "exec.log")); !strings.Contains(execLog, "'@gc_fence'") || strings.Contains(execLog, "'-u'") {
				t.Errorf("exec log %q: want the new fence published and no fence cleared", execLog)
			}
			want := []string{"observe", "start", "exec"}
			if failPublish {
				want = append(want, "observe", "stop")
			}
			if got := opsRan(t, dir); !slices.Equal(got, want) {
				t.Errorf("ops = %v, want %v", got, want)
			}
		})
	}
}

// A v2-ready welded Relaunch stops, waits for observe to read absent, then
// starts fresh; a box that never goes is an error and nothing starts.
func TestReadyWeldedRelaunchStartsFresh(t *testing.T) {
	script, dir := observePack(t, readyHandshake, freshOps)
	writeFile(t, filepath.Join(dir, "state"), boxWithToken("running", "tok-old"))
	if err := NewSeamBacked(script).(runtime.RelaunchProvider).Relaunch(t.Context(), "s", runtime.Config{Env: map[string]string{"GC_INSTANCE_TOKEN": "tok-new"}}); err != nil {
		t.Fatal(err)
	}
	if got, want := opsRan(t, dir), []string{"stop", "observe", "observe", "start"}; !slices.Equal(got, want) {
		t.Errorf("ops = %v, want %v", got, want)
	}
	if !strings.Contains(readLog(t, filepath.Join(dir, "start.stdin")), `"fresh_only":true`) {
		t.Error("relaunch start did not carry fresh_only")
	}

	stuck, stuckDir := observePack(t, readyHandshake, map[string]string{"observe": freshOps["observe"], "start": freshOps["start"], "stop": `:`})
	writeFile(t, filepath.Join(stuckDir, "state"), boxWithToken("running", "tok-old"))
	p := NewProvider(stuck)
	p.timeout = time.Second
	if err := p.Relaunch(t.Context(), "s", runtime.Config{}); err == nil {
		t.Error("Relaunch over a box that stays = nil, want an error")
	}
	if slices.Contains(opsRan(t, stuckDir), "start") {
		t.Error("relaunch started over a box that never went")
	}
}

// Kills (§13.5 13): an op run without the city it serves.
func TestCityEnvOnEveryOp(t *testing.T) {
	t.Setenv("GC_EXEC_CITY_NAME", "stale")
	t.Setenv("GC_EXEC_CITY_PATH", "")
	script, _ := observePack(t, `{"version":0,"capabilities":["proc.exec"]}`, map[string]string{
		"get-meta": `echo "$GC_EXEC_CITY_NAME|$GC_EXEC_CITY_PATH"`,
		"exec":     `cat >/dev/null; echo "$GC_EXEC_CITY_NAME|$GC_EXEC_CITY_PATH"`,
	})
	sp := NewSeamBackedForCity(script, "alpha", "/cities/alpha")
	if v, err := sp.GetMeta("s", "k"); v != "alpha|/cities/alpha" || err != nil {
		t.Errorf("get-meta saw %q, %v; want the city env", v, err)
	}
	out, code, err := NewProviderForCity(script, "alpha", "/cities/alpha").Exec(t.Context(), "s", []string{"true"})
	if strings.TrimSpace(string(out)) != "alpha|/cities/alpha" || code != 0 || err != nil {
		t.Errorf("exec saw %q (%d, %v); want the city env", out, code, err)
	}
	if v, _ := NewSeamBacked(script).GetMeta("s", "k"); v != "stale|" {
		t.Errorf("a provider for no city: get-meta saw %q, want gc's own env", v)
	}
}

// Kills (S1): the Relaunch readiness gate dropped: a welded pack that is
// not v2-ready relaunches as before, Stop then a legacy Start, with no
// `observe` and no fresh_only.
func TestNotReadyWeldedRelaunchIsLegacy(t *testing.T) {
	for _, handshake := range []string{"", `{"version":0,"capabilities":["observe.v1"]}`} {
		script, dir := observePack(t, handshake, legacyOps)
		if err := NewSeamBacked(script).(runtime.RelaunchProvider).Relaunch(t.Context(), "s", runtime.Config{}); err != nil {
			t.Fatalf("handshake %q: Relaunch = %v", handshake, err)
		}
		ran := opsRan(t, dir)
		if slices.Contains(ran, "observe") || !slices.Contains(ran, "stop") || !slices.Contains(ran, "start") {
			t.Errorf("handshake %q: ops %v, want stop then start and no observe", handshake, ran)
		}
		if strings.Contains(readLog(t, filepath.Join(dir, "start.stdin")), "fresh_only") {
			t.Errorf("handshake %q: a legacy relaunch sent fresh_only", handshake)
		}
	}
}

// Kills (S1): the 4s is-attached bound applied to a pack that is not
// v2-ready, which keeps the op timeout it had.
func TestNotReadyIsAttachedKeepsItsTimeout(t *testing.T) {
	script, _ := observePack(t, `{"version":0,"capabilities":["report-attachment"]}`, map[string]string{"is-attached": `sleep 4.6 & wait $!; echo true`})
	if attached, err := NewProvider(script).IsAttachedWithError("s"); !attached || err != nil {
		t.Fatalf("slow is-attached on a pack that is not v2-ready = %v, %v; want true, nil", attached, err)
	}
}

// Kills (NIT 4): the seam's not-ready liveness read taken from the raw
// provider instead of the seams. With is-running false and process-alive
// true the seams read no box (ProcessAlive asks the attachment, which is
// not open), where the raw provider would read a live agent.
func TestNotReadySeamLivenessReadsThroughTheSeams(t *testing.T) {
	script, _ := observePack(t, "", map[string]string{"is-running": `echo false`, "process-alive": `cat >/dev/null; echo true`})
	sp := NewSeamBacked(script)
	pn := []string{"claude"}
	got, err := sp.(runtime.LivenessObserverWithError).ObserveLivenessWithError("s", pn)
	if err != nil || got != (runtime.Liveness{}) {
		t.Fatalf("seam ObserveLivenessWithError = %+v, %v; want the seams' read, no box", got, err)
	}
	if raw := runtime.ObserveLiveness(NewProvider(script), "s", pn); raw == got {
		t.Fatalf("raw read %+v equals the seams'; this fixture pins nothing", raw)
	}
}

// Kills (NIT 3): watch-startup or the attach TTY run without the city.
func TestCityEnvOnWatchAndAttach(t *testing.T) {
	script, dir := observePack(t, "", map[string]string{
		"watch-startup": `echo "$GC_EXEC_CITY_NAME|$GC_EXEC_CITY_PATH" > "$dir/watch.env"; exit 2`,
		"attach":        `echo "$GC_EXEC_CITY_NAME|$GC_EXEC_CITY_PATH" > "$dir/attach.env"`,
	})
	p := NewProviderForCity(script, "alpha", "/cities/alpha")
	if _, _, ok, err := p.startStartupWatch(t.Context(), "s", time.Second); ok || err != nil {
		t.Fatalf("startStartupWatch = %v, %v; want unsupported", ok, err)
	}
	if err := p.Attach("s"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"watch.env", "attach.env"} {
		if got := readLog(t, filepath.Join(dir, f)); got != "alpha|/cities/alpha\n" {
			t.Errorf("%s = %q, want the city env", f, got)
		}
	}
}

// Kills (NIT 1): a collision read from an op other than `start` or
// `provision`, or from anything but exit 73 or the collision phrase.
func TestFreshCollision(t *testing.T) {
	base := errors.New("boom")
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"start exit 73", &opExitError{op: "start", code: 73, err: base}, true},
		{"provision exit 73", &opExitError{op: "provision", code: 73, err: base}, true},
		{"provision phrase", &opExitError{op: "provision", code: 1, msg: "box already exists", err: base}, true},
		{"start phrase, classified", fmt.Errorf("%w: %w", runtime.ErrSessionExists, &opExitError{op: "start", code: 1, msg: "already running", err: base}), true},
		{"start other failure", &opExitError{op: "start", code: 1, msg: "broken", err: base}, false},
		{"stop exit 73", &opExitError{op: "stop", code: 73, err: base}, false},
		{"exec phrase", &opExitError{op: "exec", code: 1, msg: "already exists", err: base}, false},
		{"bare ErrSessionExists", fmt.Errorf("%w: launch refused", runtime.ErrSessionExists), false},
	} {
		if got := freshCollision(tc.err); got != tc.want {
			t.Errorf("%s: freshCollision = %v, want %v", tc.name, got, tc.want)
		}
	}
}
