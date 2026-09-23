package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
)

// admissionFixture is a real proxied scope on disk: bd's metadata binding, its
// sidecar, and a schema-2 proxy record.
//
// The files are real because admission's file reads are its parity contract
// with bd -- ProviderRoot resolves the root bd resolves, Read and Validate
// decode the document bd wrote. A test that stubbed those would prove gc agrees
// with a fake. Only the three effects a test cannot afford are injected: the
// process table, the TCP session, and the bd fork.
type admissionFixture struct {
	t         *testing.T
	scopeRoot string
	root      string
	record    proxyendpoint.Record

	alive bool
	argv  []string
}

func newAdmissionFixture(t *testing.T, idleTimeout string) *admissionFixture {
	t.Helper()
	// bd's own environment arms must not decide the root under test.
	t.Setenv(proxyendpoint.RootPathEnv, "")
	t.Setenv(proxyendpoint.DoltDataDirEnv, "")
	t.Setenv(proxyendpoint.SharedServerModeEnv, "")
	t.Setenv(proxyendpoint.SharedServerDirEnv, "")

	scopeRoot := t.TempDir()
	beadsDir := filepath.Join(scopeRoot, ".beads")
	root := filepath.Join(beadsDir, proxyendpoint.DefaultRootDirName)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"),
		[]byte(`{"backend":"dolt","dolt_mode":"proxied-server","dolt_database":"beads"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sidecar := `{"root_path":"dolt","port":44561`
	if idleTimeout != "" {
		sidecar += `,"idle_timeout":` + idleTimeout
	}
	sidecar += `}`
	if err := os.WriteFile(proxyendpoint.SidecarPath(beadsDir), []byte(sidecar), 0o600); err != nil {
		t.Fatal(err)
	}

	f := &admissionFixture{t: t, scopeRoot: scopeRoot, root: root, alive: true}
	f.argv = []string{"/opt/beads/bd", proxyendpoint.ChildVerb, proxyendpoint.RootFlag, root}
	f.writeRecord(6001, "44556677")
	return f
}

func (f *admissionFixture) writeRecord(pid int, start string) {
	f.t.Helper()
	rootID, err := proxyendpoint.RootID(f.root)
	if err != nil {
		f.t.Fatalf("RootID(%s): %v", f.root, err)
	}
	f.record = proxyendpoint.Record{
		PID:         pid,
		Port:        44561,
		UpstreamID:  "upstream",
		Schema:      proxyendpoint.SchemaV2,
		Kind:        proxyendpoint.RecordKind,
		Birth:       proxyendpoint.BirthToken("boot-fixture", start),
		RootID:      rootID,
		ControlPort: 44562,
	}
	f.persist()
}

// corrupt rewrites the record after mutate has edited it, for the arms that
// need a document that fails validation.
func (f *admissionFixture) corrupt(mutate func(*proxyendpoint.Record)) {
	f.t.Helper()
	mutate(&f.record)
	f.persist()
}

func (f *admissionFixture) persist() {
	f.t.Helper()
	body, err := json.Marshal(f.record)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(proxyendpoint.PIDPath(f.root), body, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *admissionFixture) removeRecord() {
	f.t.Helper()
	if err := os.Remove(proxyendpoint.PIDPath(f.root)); err != nil && !os.IsNotExist(err) {
		f.t.Fatal(err)
	}
}

// processTable reports the fixture's recorded pid as bd's live supervisor for
// this root, when the fixture says it is alive.
func (f *admissionFixture) processTable() proxyendpoint.ProcessTable {
	return proxyendpoint.ProcessTable{
		Alive: func(pid int) bool { return f.alive && pid == f.record.PID },
		Argv: func(pid int) ([]string, error) {
			if !f.alive || pid != f.record.PID {
				return nil, errors.New("no such process")
			}
			return f.argv, nil
		},
		Birth: func(pid int) (string, error) {
			if !f.alive || pid != f.record.PID {
				return "", errors.New("no such process")
			}
			return f.record.Birth, nil
		},
	}
}

// admissionOps counts the bd verbs admission spent.
type admissionOps struct {
	mu       sync.Mutex
	pings    int
	recovers int
	onPing   func() error
	onRecov  func() error
}

func (o *admissionOps) Ping(context.Context, string) error {
	o.mu.Lock()
	o.pings++
	hook := o.onPing
	o.mu.Unlock()
	if hook != nil {
		return hook()
	}
	return nil
}

func (o *admissionOps) Recover(context.Context, string) error {
	o.mu.Lock()
	o.recovers++
	hook := o.onRecov
	o.mu.Unlock()
	if hook != nil {
		return hook()
	}
	return nil
}

func (o *admissionOps) counts() (int, int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.pings, o.recovers
}

// servedProbe answers with the cursors this binary pins, which is the only
// cursor pair that passes the gate.
func servedProbe(cursors proxyendpoint.Cursors, calls *int) func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
	return func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
		*calls++
		return proxyendpoint.ServedProbeForTest(cursors, proxyendpoint.CursorReality{})
	}
}

// servedProbeWithReality answers with a served endpoint whose ignored-lane
// cursor the live schema contradicts. It is the shape council A-F2 is about: the
// number ON DISK is the pinned one, and the number the linked library acts on is
// not.
func servedProbeWithReality(cursors proxyendpoint.Cursors, reality proxyendpoint.CursorReality, calls *int) func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
	return func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
		*calls++
		return proxyendpoint.ServedProbeForTest(cursors, reality)
	}
}

func pinnedCursors() proxyendpoint.Cursors {
	main, ignored := PinnedSchemaCursors()
	return proxyendpoint.Cursors{Main: main, Ignored: ignored}
}

// openIfAdmitted stands in for the library open the opener would perform. It is
// reachable ONLY through an admitted Pin, which is what makes "the gate cannot
// be bypassed" an assertion rather than a claim: Pin's fields are unexported
// and only Admit mints a non-zero one.
func openIfAdmitted(pin Pin, opened *int) {
	if pin.Admitted() {
		*opened++
	}
}

func baseAdmissionInput(f *admissionFixture, ops *admissionOps) AdmissionInput {
	return AdmissionInput{
		ScopeRoot:    f.scopeRoot,
		Database:     "beads",
		ProcessTable: f.processTable(),
		Ops:          ops,
		Observed:     NewGenerationSet(),
		Recovered:    NewGenerationSet(),
		Now:          time.Now,
		Sleep:        func(context.Context, time.Duration) error { return nil },
		SkipMemo:     true,
	}
}

// TestAdmitTable is the admission decision table.
//
// Every row asserts a verdict AND a cost, because the two are the point
// together: admission exists to decide without forking bd, and a row that
// reached the right answer by spending a fork would pass a verdict-only test
// while destroying the lane's reason to exist.
func TestAdmitTable(t *testing.T) {
	t.Run("served and equal cursors pins with no bd verbs", func(t *testing.T) {
		f := newAdmissionFixture(t, "-1")
		ops := &admissionOps{}
		probes, opened := 0, 0

		in := baseAdmissionInput(f, ops)
		in.LongLived = true
		in.Probe = servedProbe(pinnedCursors(), &probes)

		pin, err := Admit(context.Background(), in)
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
		openIfAdmitted(pin, &opened)
		if opened != 1 {
			t.Fatalf("a healthy admission did not yield an openable pin")
		}
		if probes != 1 {
			t.Errorf("probe sessions = %d, want exactly 1: a healthy endpoint costs bd one session, not two", probes)
		}
		if pings, recovers := ops.counts(); pings != 0 || recovers != 0 {
			t.Errorf("bd verbs on a healthy proxy = %d ping / %d recover, want 0/0", pings, recovers)
		}
		if pin.Port() != 44561 || pin.PoolKey().PID != 6001 {
			t.Errorf("pin = %+v, want the recorded endpoint", pin.PoolKey())
		}
		if pin.IdlePolicy().Kind != proxyendpoint.IdleNever {
			t.Errorf("idle policy = %s, want never for a sidecar that says -1", pin.IdlePolicy())
		}
		if pin.Evidence() != proxyendpoint.EvidenceArgvBirth {
			t.Errorf("evidence = %s, want argv+birth", pin.Evidence())
		}
		if pin.Cursors() != pinnedCursors() {
			t.Errorf("pinned cursors = %v, want the probed pair", pin.Cursors())
		}
		// The report the factory diagnostic is built from comes from the pin,
		// so a refusal and a pass describe the same endpoint the same way.
		if got := pin.Report(); got.Endpoint.Generation != pin.Generation() || got.IdlePolicy != "never(sidecar)" {
			t.Errorf("Report() = %+v, want the pin's own account", got)
		}
	})

	t.Run("main lane ahead refuses without opening anything", func(t *testing.T) {
		f := newAdmissionFixture(t, "-1")
		ops := &admissionOps{}
		probes, opened := 0, 0
		cursors := pinnedCursors()
		cursors.Main++

		in := baseAdmissionInput(f, ops)
		in.Probe = servedProbe(cursors, &probes)

		pin, err := Admit(context.Background(), in)
		openIfAdmitted(pin, &opened)
		if opened != 0 {
			t.Fatal("a schema-skewed database yielded an openable pin; the library would have migrated it on open")
		}
		verdict, ok := ProxiedVerdictOf(err)
		if !ok {
			t.Fatalf("Admit error = %v, want a typed verdict", err)
		}
		if verdict.Verdict != ProxiedVerdictSchemaSkew || verdict.Lane != ProxiedSkewLaneMain || verdict.Dir != ProxiedSkewDirAhead {
			t.Fatalf("verdict = %+v, want schema_skew{main,ahead}", verdict)
		}
		if !verdict.Terminal() {
			t.Error("schema_skew must be terminal: a retry cannot move a migration cursor")
		}
		if pings, recovers := ops.counts(); pings != 0 || recovers != 0 {
			t.Errorf("a cursor mismatch spent %d ping / %d recover, want 0/0", pings, recovers)
		}
	})

	t.Run("ignored lane behind refuses", func(t *testing.T) {
		f := newAdmissionFixture(t, "-1")
		probes := 0
		cursors := pinnedCursors()
		cursors.Ignored--

		in := baseAdmissionInput(f, &admissionOps{})
		in.Probe = servedProbe(cursors, &probes)

		_, err := Admit(context.Background(), in)
		verdict, ok := ProxiedVerdictOf(err)
		if !ok {
			t.Fatalf("Admit error = %v, want a typed verdict", err)
		}
		if verdict.Verdict != ProxiedVerdictSchemaSkew || verdict.Lane != ProxiedSkewLaneIgnored || verdict.Dir != ProxiedSkewDirBehind {
			t.Fatalf("verdict = %+v, want schema_skew{ignored,behind}", verdict)
		}
	})

	// Council A-F2. The cursors on disk are EQUAL on both lanes and this row
	// used to admit. The linked library does not read those numbers: with
	// `leases.granted_node` absent it computes min(26, 11) = 11, decides the
	// ignored lane is behind, and — because the proxied open is writable, bd's
	// own shared-store migrate gate consults the MAIN lane only, and MigrateUp
	// then calls ignoredSource.migrate unconditionally — replays ignored
	// 0012-0025 against a database bd owns, from a handle gc opened purely to
	// read. Nothing may be openable here.
	t.Run("a clamped ignored lane refuses at equal on-disk cursors", func(t *testing.T) {
		f := newAdmissionFixture(t, "-1")
		ops := &admissionOps{}
		probes, opened := 0, 0
		reality := proxyendpoint.CursorReality{
			Limited: true,
			Floor:   proxyendpoint.IgnoredSentinelColumnFloor,
			Missing: proxyendpoint.IgnoredSentinelColumnTable + "." + proxyendpoint.IgnoredSentinelColumnName,
		}

		in := baseAdmissionInput(f, ops)
		in.LongLived = true
		in.Probe = servedProbeWithReality(pinnedCursors(), reality, &probes)

		pin, err := Admit(context.Background(), in)
		openIfAdmitted(pin, &opened)
		if opened != 0 {
			t.Fatal("a database whose ignored lane the library disbelieves yielded an openable pin; " +
				"the library would have replayed ignored 0012-0025 against bd's database on open")
		}
		verdict, ok := ProxiedVerdictOf(err)
		if !ok {
			t.Fatalf("Admit error = %v, want a typed verdict", err)
		}
		if verdict.Verdict != ProxiedVerdictSchemaSkew || verdict.Lane != ProxiedSkewLaneIgnored || verdict.Dir != ProxiedSkewDirBehind {
			t.Fatalf("verdict = %+v, want schema_skew{ignored,behind}", verdict)
		}
		if !verdict.Terminal() {
			t.Error("a clamped lane is a fact about the database, so the refusal must be terminal")
		}
		if !strings.Contains(verdict.Error(), "leases.granted_node") {
			t.Errorf("the refusal does not name the missing sentinel, so an operator cannot act on it: %v", verdict)
		}
		if pings, recovers := ops.counts(); pings != 0 || recovers != 0 {
			t.Errorf("a cursor-reality mismatch spent %d ping / %d recover, want 0/0", pings, recovers)
		}
	})

	t.Run("dead record costs one ping then pins", func(t *testing.T) {
		f := newAdmissionFixture(t, "-1")
		f.alive = false
		probes := 0
		ops := &admissionOps{onPing: func() error {
			// bd adopted its proxy: the recorded process is live again.
			f.alive = true
			return nil
		}}

		in := baseAdmissionInput(f, ops)
		in.Probe = servedProbe(pinnedCursors(), &probes)

		pin, err := Admit(context.Background(), in)
		if err != nil {
			t.Fatalf("Admit after a dead record: %v", err)
		}
		if !pin.Admitted() {
			t.Fatal("Admit returned no error and no pin")
		}
		if pings, recovers := ops.counts(); pings != 1 || recovers != 0 {
			t.Errorf("bd verbs = %d ping / %d recover, want exactly 1/0", pings, recovers)
		}
		if probes != 1 {
			t.Errorf("probe sessions = %d, want 1: the dead pass never reached the wire", probes)
		}
	})

	t.Run("refused on a live same generation drains then re-pins", func(t *testing.T) {
		f := newAdmissionFixture(t, "-1")
		ops := &admissionOps{}
		probes, sleeps := 0, 0

		in := baseAdmissionInput(f, ops)
		in.LongLived = true
		in.Sleep = func(context.Context, time.Duration) error {
			sleeps++
			// The drain finishes: bd's replacement proxy publishes a new
			// generation at the same root.
			f.writeRecord(6002, "88990011")
			return nil
		}
		in.ProcessTable = f.processTable()
		in.Probe = func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
			probes++
			if probes == 1 {
				return proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeRefused}
			}
			return proxyendpoint.ServedProbeForTest(pinnedCursors(), proxyendpoint.CursorReality{})
		}

		pin, err := Admit(context.Background(), in)
		if err != nil {
			t.Fatalf("Admit across a drain: %v", err)
		}
		if pin.PoolKey().PID != 6002 {
			t.Errorf("re-pinned generation = %s, want the replacement proxy", pin.Generation())
		}
		if sleeps != 1 {
			t.Errorf("drain polls = %d, want 1", sleeps)
		}
		if pings, recovers := ops.counts(); pings != 0 || recovers != 0 {
			t.Errorf("a drain spent %d ping / %d recover, want 0/0: a proxy on its way down needs waiting out, not a bd fork", pings, recovers)
		}
	})

	t.Run("a one-shot refuses a draining proxy instead of waiting", func(t *testing.T) {
		f := newAdmissionFixture(t, "-1")
		in := baseAdmissionInput(f, &admissionOps{})
		in.LongLived = false
		in.Sleep = func(context.Context, time.Duration) error {
			t.Error("a one-shot open waited out a drain; the command in front of it would rather run on BdStore now")
			return nil
		}
		in.Probe = func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
			return proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeRefused}
		}

		_, err := Admit(context.Background(), in)
		verdict, ok := ProxiedVerdictOf(err)
		if !ok {
			t.Fatalf("Admit error = %v, want a typed verdict", err)
		}
		if verdict.Verdict != ProxiedVerdictDraining || verdict.Terminal() {
			t.Fatalf("verdict = %+v, want a NON-terminal draining", verdict)
		}
	})

	t.Run("budget expiry mid drain is draining and non terminal", func(t *testing.T) {
		f := newAdmissionFixture(t, "-1")
		in := baseAdmissionInput(f, &admissionOps{})
		in.LongLived = true
		in.Sleep = func(context.Context, time.Duration) error { return context.DeadlineExceeded }
		in.Probe = func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
			return proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeRefused}
		}

		_, err := Admit(context.Background(), in)
		verdict, ok := ProxiedVerdictOf(err)
		if !ok {
			t.Fatalf("Admit error = %v, want a typed verdict", err)
		}
		if verdict.Verdict != ProxiedVerdictDraining {
			t.Fatalf("verdict = %q, want draining", verdict.Verdict)
		}
		if verdict.Terminal() {
			t.Fatal("a budget that expired says nothing about the proxy, so it must never demote a handle permanently")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("the refusal lost its cause: %v", err)
		}
	})

	t.Run("zombie ladder spends one ping and one recover per generation", func(t *testing.T) {
		f := newAdmissionFixture(t, "-1")
		ops := &admissionOps{}
		probes := 0
		var waits []time.Duration

		in := baseAdmissionInput(f, ops)
		in.Probe = func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
			probes++
			return proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeAcceptedNoGreeting}
		}
		// The ladder's DEPTH and SPACING are the cost the ladder exists to buy,
		// and neither was asserted: probes was counted and never read, and
		// Sleep was a no-op, so admissionNoGreetingSpacing was unexercised
		// (council C-F9). Recording the waits is what makes "three times across
		// at least two seconds" a property rather than a comment.
		in.Sleep = func(_ context.Context, d time.Duration) error {
			waits = append(waits, d)
			return nil
		}

		_, err := Admit(context.Background(), in)
		verdict, ok := ProxiedVerdictOf(err)
		if !ok {
			t.Fatalf("Admit error = %v, want a typed verdict", err)
		}
		if verdict.Verdict != ProxiedVerdictProxyZombie || !verdict.Terminal() {
			t.Fatalf("verdict = %+v, want a terminal proxy_zombie", verdict)
		}
		pings, recovers := ops.counts()
		if pings != 1 || recovers != 1 {
			t.Fatalf("the ladder spent %d ping / %d recover, want exactly 1/1", pings, recovers)
		}

		// A second admission on the same generation must not buy either rung
		// again: bd has been asked and has not fixed it.
		_, err = Admit(context.Background(), in)
		if verdict, ok := ProxiedVerdictOf(err); !ok || verdict.Verdict != ProxiedVerdictProxyZombie {
			t.Fatalf("second admission = %v, want proxy_zombie", err)
		}
		if pings, recovers := ops.counts(); pings != 1 || recovers != 1 {
			t.Errorf("a second zombie pass spent more verbs: %d ping / %d recover, want 1/1", pings, recovers)
		}

		// Four passes in total, and the count is the ladder's shape rather than
		// a number: the FIRST Admit above runs three — one that spends the ping
		// and re-admits, one that spends the recover and re-admits, and one
		// that finds both rungs spent and answers proxy_zombie — and the second
		// Admit runs one, which finds both rungs spent immediately. Each pass
		// costs admissionNoGreetingAttempts probe sessions: admitOnce's own,
		// plus the re-probes escalateZombie walks before it spends anything.
		const passes = 4
		if want := passes * admissionNoGreetingAttempts; probes != want {
			t.Fatalf("the no-greeting ladder ran %d probe sessions across %d passes, want %d "+
				"(%d per pass). A ladder that escalated on the FIRST silent probe would fork bd for "+
				"every proxy that is merely mid-restart.",
				probes, passes, want, admissionNoGreetingAttempts)
		}
		if want := passes * (admissionNoGreetingAttempts - 1); len(waits) != want {
			t.Fatalf("the ladder waited %d times, want %d: the re-probes must be SPACED, "+
				"or three probes in a microsecond is the same as one", len(waits), want)
		}
		for i, wait := range waits {
			if wait != admissionNoGreetingSpacing {
				t.Fatalf("wait %d was %s, want admissionNoGreetingSpacing (%s)", i, wait, admissionNoGreetingSpacing)
			}
		}
		// The span the comment at admissionNoGreetingAttempts promises, stated
		// as an assertion so a retuned constant has to face it.
		if span := time.Duration(admissionNoGreetingAttempts-1) * admissionNoGreetingSpacing; span < 2*time.Second {
			t.Fatalf("one no-greeting pass spans %s, want at least 2s: a proxy mid-restart accepts and "+
				"stays silent for a beat, and escalating inside that beat forks bd for a proxy that was "+
				"about to answer", span)
		}
	})

	t.Run("a foreign root id refuses without dialing", func(t *testing.T) {
		f := newAdmissionFixture(t, "-1")
		f.corrupt(func(rec *proxyendpoint.Record) {
			rec.RootID = "0000000000000000000000000000000000000000000000000000000000000000"
		})
		ops := &admissionOps{}

		in := baseAdmissionInput(f, ops)
		in.Probe = func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
			t.Fatal("admission dialed an endpoint whose record does not validate for this root")
			return proxyendpoint.ProbeResult{}
		}

		_, err := Admit(context.Background(), in)
		verdict, ok := ProxiedVerdictOf(err)
		if !ok {
			t.Fatalf("Admit error = %v, want a typed verdict", err)
		}
		if verdict.Verdict != ProxiedVerdictNotOurs || !verdict.Terminal() {
			t.Fatalf("verdict = %+v, want a terminal not_ours", verdict)
		}
		if pings, recovers := ops.counts(); pings != 0 || recovers != 0 {
			t.Errorf("a foreign record spent %d ping / %d recover, want 0/0", pings, recovers)
		}
	})

	t.Run("a pre schema 2 record refuses as legacy without dialing", func(t *testing.T) {
		f := newAdmissionFixture(t, "-1")
		f.corrupt(func(rec *proxyendpoint.Record) { rec.Schema = 1 })

		in := baseAdmissionInput(f, &admissionOps{})
		in.Probe = func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
			t.Fatal("admission dialed a record with no birth token, so no generation could be established")
			return proxyendpoint.ProbeResult{}
		}

		_, err := Admit(context.Background(), in)
		if verdict, ok := ProxiedVerdictOf(err); !ok || verdict.Verdict != ProxiedVerdictLegacySchema {
			t.Fatalf("Admit error = %v, want legacy_schema", err)
		}
	})

	t.Run("an absent record costs one ping per process", func(t *testing.T) {
		f := newAdmissionFixture(t, "-1")
		f.removeRecord()
		ops := &admissionOps{}
		in := baseAdmissionInput(f, ops)
		in.Probe = func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
			t.Fatal("admission dialed a scope with no proxy record")
			return proxyendpoint.ProbeResult{}
		}

		if _, err := Admit(context.Background(), in); err == nil {
			t.Fatal("Admit succeeded with no proxy record")
		}
		if pings, _ := ops.counts(); pings != 1 {
			t.Fatalf("an absent record spent %d pings, want exactly 1", pings)
		}
		// The second open in the same process must not buy the same answer.
		if _, err := Admit(context.Background(), in); err == nil {
			t.Fatal("Admit succeeded with no proxy record")
		}
		if pings, _ := ops.counts(); pings != 1 {
			t.Errorf("a second open on the same missing proxy spent %d pings, want still 1", pings)
		}
	})

	t.Run("a finite idle policy refuses a long lived open", func(t *testing.T) {
		// bd's provider substitutes a 30s window for a sidecar with no
		// idle_timeout, so this is the shape an OPERATOR-initialized proxied
		// scope has -- and the one gc must not hold a resident handle against.
		f := newAdmissionFixture(t, "")
		ops := &admissionOps{}
		probes := 0

		// The memo is LIVE here (council C-F2). Every test that pinned this
		// refusal used to set SkipMemo, which is the mechanism production does
		// not have -- and the one the refusal was being bypassed through.
		ForgetProxiedPin(f.scopeRoot, "beads")
		t.Cleanup(func() { ForgetProxiedPin(f.scopeRoot, "beads") })
		in := baseAdmissionInput(f, ops)
		in.SkipMemo = false
		in.LongLived = true
		in.Probe = servedProbe(pinnedCursors(), &probes)

		_, err := Admit(context.Background(), in)
		verdict, ok := ProxiedVerdictOf(err)
		if !ok {
			t.Fatalf("Admit error = %v, want a typed verdict", err)
		}
		if verdict.Verdict != ProxiedVerdictIdlePolicyFinite || !verdict.Terminal() {
			t.Fatalf("verdict = %+v, want a terminal idle_policy_finite", verdict)
		}
		if probes != 0 {
			t.Errorf("the idle rule dialed %d times; it is decided from the sidecar and argv alone", probes)
		}

		// The SAME scope is fine for a one-shot: nothing is held across the
		// idle window.
		in.LongLived = false
		pin, err := Admit(context.Background(), in)
		if err != nil {
			t.Fatalf("a one-shot open on a finite-idle proxy: %v", err)
		}
		if pin.IdlePolicy().Kind != proxyendpoint.IdleFinite {
			t.Errorf("idle policy = %s, want finite", pin.IdlePolicy())
		}

		// And the one-shot's memoized pass must not become the long-lived
		// answer. This is the ORDER production runs it in --
		// cmd/gc/main.go's openStoreAtForCity is longLived=false and
		// cmd/gc/api_state.go is LongLived=true, in one binary.
		in.LongLived = true
		_, err = Admit(context.Background(), in)
		verdict, ok = ProxiedVerdictOf(err)
		if !ok || verdict.Verdict != ProxiedVerdictIdlePolicyFinite {
			t.Fatalf("a long-lived open after a one-shot memoized its pass = %v, want idle_policy_finite; "+
				"gc would hold a resident handle on a proxy bd is going to retire", err)
		}
	})

	t.Run("the live supervisor argv outranks the sidecar", func(t *testing.T) {
		// An operator edited the sidecar to say never under a proxy bd started
		// with a finite window. Only the argv describes the running process.
		f := newAdmissionFixture(t, "-1")
		f.argv = append(f.argv, proxyendpoint.IdleTimeoutFlag, "30s")
		in := baseAdmissionInput(f, &admissionOps{})
		in.ProcessTable = f.processTable()
		in.LongLived = true
		probes := 0
		in.Probe = servedProbe(pinnedCursors(), &probes)

		_, err := Admit(context.Background(), in)
		if verdict, ok := ProxiedVerdictOf(err); !ok || verdict.Verdict != ProxiedVerdictIdlePolicyFinite {
			t.Fatalf("Admit error = %v, want idle_policy_finite from the supervisor's own argv", err)
		}
	})
}

// TestAdmitMemoHoldsRepeatedOpensToOneProbeSession pins the memo's reason to
// exist: `gc doctor` opens a scope 17+ times in one run, and admission's
// cheapest healthy path still costs one probe SESSION -- an accepted TCP
// connection bd's idle watcher counts, which cannot arm while one is open.
//
// It is keyed on the two files the answer derives from, so a proxy that moved
// invalidates the entry immediately whatever the TTL says. That is the half
// worth testing: a time-only memo would keep serving a generation that no
// longer exists.
func TestAdmitMemoHoldsRepeatedOpensToOneProbeSession(t *testing.T) {
	f := newAdmissionFixture(t, "-1")
	ForgetProxiedPin(f.scopeRoot, "beads")
	t.Cleanup(func() { ForgetProxiedPin(f.scopeRoot, "beads") })

	probes := 0
	in := baseAdmissionInput(f, &admissionOps{})
	in.SkipMemo = false
	in.Probe = servedProbe(pinnedCursors(), &probes)

	for i := 0; i < 5; i++ {
		if _, err := Admit(context.Background(), in); err != nil {
			t.Fatalf("Admit #%d: %v", i, err)
		}
	}
	if probes != 1 {
		t.Fatalf("five opens cost %d probe sessions, want 1", probes)
	}

	// A new generation at the same root must miss: the record's stamp changed.
	time.Sleep(10 * time.Millisecond)
	f.writeRecord(6002, "88990011")
	in.ProcessTable = f.processTable()
	pin, err := Admit(context.Background(), in)
	if err != nil {
		t.Fatalf("Admit after a generation change: %v", err)
	}
	if probes != 2 {
		t.Errorf("probe sessions after a generation change = %d, want 2: the memo served a proxy that no longer exists", probes)
	}
	if pin.PoolKey().PID != 6002 {
		t.Errorf("pin = %s, want the new generation", pin.Generation())
	}

	// SkipMemo is what the guard tick uses: a tick that re-admitted out of the
	// memo it populated would be reading its own answer back.
	in.SkipMemo = true
	if _, err := Admit(context.Background(), in); err != nil {
		t.Fatalf("Admit with SkipMemo: %v", err)
	}
	if probes != 3 {
		t.Errorf("probe sessions with SkipMemo = %d, want 3", probes)
	}

	// The lane is part of the key (council C-F2), so a one-shot's entry is not
	// a long-lived open's answer. That costs one extra session per scope per
	// lane, which is the price of the memo answering the question it was asked:
	// the doctor run the memo exists for opens one lane, so its 17-to-1 saving
	// is untouched.
	in.SkipMemo = false
	in.LongLived = true
	if _, err := Admit(context.Background(), in); err != nil {
		t.Fatalf("Admit(long-lived): %v", err)
	}
	if probes != 4 {
		t.Fatalf("a long-lived open read a ONE-SHOT memo entry (probe sessions = %d, want 4)", probes)
	}
	for i := 0; i < 3; i++ {
		if _, err := Admit(context.Background(), in); err != nil {
			t.Fatalf("Admit(long-lived) #%d: %v", i, err)
		}
	}
	if probes != 4 {
		t.Fatalf("the long-lived lane is not memoized at all (probe sessions = %d, want 4)", probes)
	}

	// And a generation change forgets BOTH lanes: the contradiction the tick
	// just proved is not one lane's.
	ForgetProxiedPin(f.scopeRoot, "beads")
	in.LongLived = false
	if _, err := Admit(context.Background(), in); err != nil {
		t.Fatalf("Admit after ForgetProxiedPin: %v", err)
	}
	if probes != 5 {
		t.Fatalf("ForgetProxiedPin left the one-shot lane memoized (probe sessions = %d, want 5)", probes)
	}
}

// TestAdmitRefusesWithoutADatabaseName pins the one input that would make the
// gate pass against nothing. The cursors are DATABASE()-scoped, so a probe with
// no database selected reports SERVED with both cursors at zero -- which
// compares unequal to the pinned pair today, and would compare EQUAL against a
// library pinned at zero. The refusal is here rather than in a comment.
func TestAdmitRefusesWithoutADatabaseName(t *testing.T) {
	f := newAdmissionFixture(t, "-1")
	in := baseAdmissionInput(f, &admissionOps{})
	in.Database = ""
	in.Probe = func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
		t.Fatal("admission probed with no database selected")
		return proxyendpoint.ProbeResult{}
	}

	_, err := Admit(context.Background(), in)
	if verdict, ok := ProxiedVerdictOf(err); !ok || verdict.Verdict != ProxiedVerdictNoOwnershipRecord {
		t.Fatalf("Admit with no database = %v, want a typed refusal", err)
	}
}

// TestZeroPinCannotBeOpened is the structural half of the gate.
//
// Pin's fields are unexported and only Admit mints a non-zero one, so a caller
// cannot reach the proxied opener by constructing a pin -- "somebody built the
// env map by hand" is unreachable rather than merely reviewed.
func TestZeroPinCannotBeOpened(t *testing.T) {
	var pin Pin
	opened := 0
	openIfAdmitted(pin, &opened)
	if opened != 0 {
		t.Fatal("the zero Pin reports itself admitted")
	}
	if pin.Port() != 0 || pin.Generation() != "0:" || pin.Root() != "" || pin.Database() != "" {
		t.Errorf("the zero Pin carries an endpoint: %+v", pin.PoolKey())
	}
}

// TestProxiedPinMemoTTLIsCapped is council A-F3's bound.
//
// The memo's stamp fingerprints proxy.pid and the sidecar, and a migration
// writes neither — so no file fingerprint can invalidate an entry when somebody
// runs `bd migrate` inside the TTL. Re-probing on a hit is not available: the
// cursor read IS the probe session, and a memo that cost what it saves has no
// reason to exist. So the exposure is bounded by time, and the TTL had no
// ceiling: it was the guard interval, and GC_BEADS_PROXIED_GUARD_INTERVAL has a
// floor and no upper bound, so asking for a quieter ticker also asked the memo
// to trust a schema answer for that long.
func TestProxiedPinMemoTTLIsCapped(t *testing.T) {
	t.Run("a quiet guard interval does not extend the memo", func(t *testing.T) {
		t.Setenv(proxiedGuardIntervalEnv, "1h")
		if got := proxiedGuardInterval(); got != time.Hour {
			t.Fatalf("proxiedGuardInterval() = %s, want 1h; this test is not driving the knob", got)
		}
		if got := proxiedPinMemoTTL(); got != proxiedPinMemoMaxTTL {
			t.Fatalf("the memo TTL is %s for a 1h guard interval, want the %s cap: a memoized schema "+
				"answer would be trusted for an hour, and no file fingerprint can see a migration",
				got, proxiedPinMemoMaxTTL)
		}
	})

	t.Run("a tick faster than the cap still bounds the memo", func(t *testing.T) {
		t.Setenv(proxiedGuardIntervalEnv, "2s")
		if got := proxiedPinMemoTTL(); got != 2*time.Second {
			t.Fatalf("the memo TTL is %s for a 2s guard interval, want 2s: the memo must never hold an "+
				"answer longer than the tick that would have re-checked it", got)
		}
	})
}

// TestGenerationSetRungsExpire is council A-F5.
//
// The escalation ledgers were process-lifetime sets. On a one-shot command that
// is indistinguishable from the design's "dedupe the many opens of one
// command"; on a controller or an api server it is not, and the difference is
// an operator-visible fault: a generation pinged at boot is still "spent" two
// hours later, so when that generation's Dolt child is OOM-killed the ladder
// skips the ping rung it would have been fixed by and escalates straight to
// `bd dolt stop` — which on a city root takes down the one proxy and Dolt child
// serving hq and every other rig, under live agents.
func TestGenerationSetRungsExpire(t *testing.T) {
	now := time.Now()
	set := NewGenerationSet()
	set.now = func() time.Time { return now }

	if !set.Add("6001:abcd") {
		t.Fatal("the first Add did not report the generation as new")
	}
	if set.Add("6001:abcd") {
		t.Fatal("a second Add inside the TTL spent the rung again; one command's many opens must share it")
	}
	if !set.Has("6001:abcd") {
		t.Fatal("Has does not see a generation recorded a moment ago")
	}

	// One second before the boundary: still spent, because the whole point is
	// that a burst of opens shares one answer.
	now = now.Add(generationMemoTTL - time.Second)
	if set.Add("6001:abcd") {
		t.Fatal("the rung expired early")
	}

	// And after it: a NEW incident, on a generation whose rung was spent long
	// ago, gets its ping.
	now = now.Add(2 * time.Second)
	if set.Has("6001:abcd") {
		t.Error("Has reports an expired rung as spent")
	}
	if !set.Add("6001:abcd") {
		t.Fatalf("a rung spent %s ago is still spent; a proxy that goes silent hours after gc pinged it "+
			"skips the ping and escalates straight to `bd dolt stop`", generationMemoTTL)
	}

	// Release is for an incident that is over: it frees the rung at once.
	if !set.Add("7001:beef") {
		t.Fatal("the first Add of a fresh generation did not report it as new")
	}
	if set.Add("7001:beef") {
		t.Fatal("the rung was not recorded, so Release below would prove nothing")
	}
	set.Release("7001:beef")
	if !set.Add("7001:beef") {
		t.Fatal("Release did not free the rung")
	}

	// Backoff is for a verb that FAILED (council pr2 D-F9): the rung is held,
	// briefly, and it is marked as a failure rather than as a spend.
	set.Backoff("7001:beef", failedPingBackoff)
	if remaining, backing := set.BackingOff("7001:beef"); !backing || remaining != failedPingBackoff {
		t.Fatalf("BackingOff = (%s, %v), want (%s, true)", remaining, backing, failedPingBackoff)
	}
	if set.Add("7001:beef") {
		t.Fatal("a failed verb's rung was spendable again at once; every open would re-fork bd")
	}
	now = now.Add(failedPingBackoff)
	if _, backing := set.BackingOff("7001:beef"); backing {
		t.Fatal("the backoff did not run out")
	}
	if !set.Add("7001:beef") {
		t.Fatal("a failed verb's rung was still held after its backoff; the scope is poisoned")
	}
	if _, backing := set.BackingOff("7001:beef"); backing {
		t.Fatal("a fresh spend still reads as a failed one")
	}
}

// TestZombieLadderDoesNotRecoverOnAFailedPing is the second half of A-F5.
//
// A provider ping can fail for reasons that are entirely gc's — the lifecycle
// semaphore, the op budget — and none of them is evidence that bd cannot make
// its proxy healthy. Cascading into `bd dolt stop` in the same pass spends the
// most destructive rung in the ladder on gc's own contention.
//
// The failure here is UNMARKED, which is what such a failure is: bd never ran
// to an answer. A failure bd itself reported is the other arm, and it is
// TestZombieLadderRecoversWhenBdReportsThePingFailed.
func TestZombieLadderDoesNotRecoverOnAFailedPing(t *testing.T) {
	f := newAdmissionFixture(t, "-1")
	ops := &admissionOps{onPing: func() error {
		return errors.New("provider op timed out waiting on the lifecycle semaphore")
	}}

	in := baseAdmissionInput(f, ops)
	in.Probe = func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
		return proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeAcceptedNoGreeting}
	}
	clock := time.Now()
	in.Observed.now = func() time.Time { return clock }

	_, err := Admit(context.Background(), in)
	verdict, ok := ProxiedVerdictOf(err)
	if !ok {
		t.Fatalf("Admit error = %v, want a typed verdict", err)
	}
	if verdict.Terminal() {
		t.Errorf("a ping that failed on gc's own semaphore says nothing about the proxy, "+
			"so the refusal must be non-terminal: %v", verdict)
	}
	pings, recovers := ops.counts()
	if recovers != 0 {
		t.Fatalf("a failed ping cascaded into %d recover(s) in the same pass; `bd dolt stop` on a city "+
			"root takes down the proxy serving hq and every rig", recovers)
	}
	if pings != 1 {
		t.Errorf("the ladder spent %d ping(s), want 1", pings)
	}

	// The rung is held for a short backoff, not released (council pr2 D-F9).
	// `gc doctor` opens one scope seventeen times; with a release each of them
	// re-forked the failing ping — on a box whose lifecycle semaphore was the
	// likely reason it failed. Inside the backoff: no ping, no recover, and a
	// non-terminal answer.
	for open := 2; open <= 17; open++ {
		_, err := Admit(context.Background(), in)
		if verdict, ok := ProxiedVerdictOf(err); !ok || verdict.Terminal() {
			t.Fatalf("open %d inside the backoff = %v, want a non-terminal verdict", open, err)
		}
	}
	if pings, recovers := ops.counts(); pings != 1 || recovers != 0 {
		t.Fatalf("seventeen opens inside the backoff spent %d ping(s) and %d recover(s), want 1 and 0: "+
			"a failed ping must be neither re-forked on every open nor escalated on", pings, recovers)
	}

	// Once it runs out, a later open — when the semaphore is free — may ask
	// bd again instead of finding the scope poisoned.
	clock = clock.Add(failedPingBackoff)
	ops.onPing = nil
	if _, err := Admit(context.Background(), in); err == nil {
		t.Fatal("the admission after the backoff admitted a proxy that still never greets")
	}
	if pings, _ := ops.counts(); pings != 2 {
		t.Fatalf("the open after the backoff spent %d ping(s) in total, want 2: a ping that failed for gc's own "+
			"reason must not hold the rung for the whole TTL", pings)
	}
}

// TestZombieLadderRecoversWhenBdReportsThePingFailed is round3 review
// (completeness): the zombie a real bd produces.
//
// SIGTERM a proxied scope's Dolt child and it exits 0, so bd's supervisor never
// notices and the proxy lives on: its data port accepts and never greets. `bd
// ping` against it adopts the proxy, fails its SELECT 1 and exits 1 — against
// a real zombie the ping can only FAIL. The rows that pinned the ladder
// scripted a ping that SUCCEEDED on a zombie, a shape real bd never produces,
// and after A-F5 every failed ping took the backoff arm; together, a real
// zombie was pinged once per backoff window for ever and never recovered, and
// the child-term-zombie acceptance row (2 pings, 1 `bd dolt stop`) could not
// pass. A failure bd reported is the design's trigger for the recover rung
// (v2 3.4, F13a/F22), and it is spent in the same pass.
func TestZombieLadderRecoversWhenBdReportsThePingFailed(t *testing.T) {
	bdSaysNo := func() error {
		return ProviderReportedFailure(errors.New("provider-owned beads probe: Error: ping: invalid connection"))
	}
	silent := proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeAcceptedNoGreeting}

	t.Run("bd's recover restores the proxy: one ping, one recover, admitted in the same Admit", func(t *testing.T) {
		f := newAdmissionFixture(t, "-1")
		zombie := f.record
		recovered := false
		ops := &admissionOps{onPing: bdSaysNo, onRecov: func() error {
			// `bd dolt stop` then `bd ping`: a new generation, which greets.
			f.writeRecord(6002, "55667788")
			recovered = true
			return nil
		}}
		in := baseAdmissionInput(f, ops)
		in.Probe = func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
			if !recovered {
				return silent
			}
			return proxyendpoint.ServedProbeForTest(pinnedCursors(), proxyendpoint.CursorReality{})
		}

		pin, err := Admit(context.Background(), in)
		if err != nil {
			pings, recovers := ops.counts()
			t.Fatalf("Admit = %v after %d ping(s) and %d recover(s): a zombie whose ping bd reported as failed "+
				"was never recovered, so every open of this scope forks bd against a proxy nobody will fix", err, pings, recovers)
		}
		if pings, recovers := ops.counts(); pings != 1 || recovers != 1 {
			t.Fatalf("the ladder spent %d ping(s) and %d recover(s), want exactly 1 and 1", pings, recovers)
		}
		if pin.PoolKey().PID != 6002 {
			t.Fatalf("admitted pid %d, want the generation the recover produced (6002, not the zombie's %d)",
				pin.PoolKey().PID, zombie.PID)
		}
	})

	t.Run("a recover that does not fix it is terminal, and neither rung is spent twice", func(t *testing.T) {
		f := newAdmissionFixture(t, "-1")
		ops := &admissionOps{onPing: bdSaysNo}
		in := baseAdmissionInput(f, ops)
		in.Probe = func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult { return silent }

		_, err := Admit(context.Background(), in)
		verdict, ok := ProxiedVerdictOf(err)
		if !ok || verdict.Verdict != ProxiedVerdictProxyZombie || !verdict.Terminal() {
			t.Fatalf("Admit = %v, want a terminal proxy_zombie: bd has been asked to ping and to recover and has not fixed it", err)
		}
		if pings, recovers := ops.counts(); pings != 1 || recovers != 1 {
			t.Fatalf("the ladder spent %d ping(s) and %d recover(s), want exactly 1 and 1", pings, recovers)
		}
		if _, err := Admit(context.Background(), in); err == nil {
			t.Fatal("a second admission of the same zombie succeeded")
		}
		if pings, recovers := ops.counts(); pings != 1 || recovers != 1 {
			t.Fatalf("a second admission of the same generation spent more: %d ping(s), %d recover(s), want 1 and 1", pings, recovers)
		}
	})

	t.Run("the mark survives wrapping, and marks nothing else", func(t *testing.T) {
		cause := errors.New("provider-owned beads probe: exit status 1")
		marked := ProviderReportedFailure(cause)
		if marked.Error() != cause.Error() {
			t.Errorf("the mark changed the error's text: %q, want %q", marked.Error(), cause.Error())
		}
		if !errors.Is(fmt.Errorf("ping scope: %w", marked), ErrProviderReportedFailure) {
			t.Error("a %w wrap between the provider op and the ladder lost the mark")
		}
		if !errors.Is(marked, cause) {
			t.Error("the mark hid its cause from errors.Is")
		}
		if errors.Is(cause, ErrProviderReportedFailure) {
			t.Error("an unmarked failure reads as one bd reported")
		}
		if ProviderReportedFailure(nil) != nil {
			t.Error("marking nil produced an error")
		}

		// And through the ladder: the same failure wrapped once more still
		// recovers in the same pass.
		f := newAdmissionFixture(t, "-1")
		ops := &admissionOps{onPing: func() error { return fmt.Errorf("ping %s: %w", f.scopeRoot, bdSaysNo()) }}
		in := baseAdmissionInput(f, ops)
		in.Probe = func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult { return silent }
		_, _ = Admit(context.Background(), in)
		if pings, recovers := ops.counts(); pings != 1 || recovers != 1 {
			t.Fatalf("a wrapped bd-reported failure spent %d ping(s) and %d recover(s), want 1 and 1", pings, recovers)
		}
	})
}

// expiringContext is a live context the test ends on demand, with the error
// of its choice. It makes "gc's own clock ran out DURING the recover" a fact of
// the fixture rather than a race against the wall clock on a loaded box.
type expiringContext struct {
	context.Context
	once sync.Once
	mu   sync.Mutex
	err  error
	done chan struct{}
}

func newExpiringContext() *expiringContext {
	return &expiringContext{Context: context.Background(), done: make(chan struct{})}
}

func (c *expiringContext) Done() <-chan struct{} { return c.done }

func (c *expiringContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *expiringContext) expire(err error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		close(c.done)
	})
}

// TestZombieLadderEndsTheLaneOnlyOnARecoverBdRefused is round4 recheck M1.
//
// The marked-ping arm sends a real zombie to the recover rung, and a recover
// failure used to be a terminal proxy_zombie whatever caused it. The read
// path's reopen runs this ladder under the read's 10s retry budget, a recover
// is `bd dolt stop` plus a `bd ping` that cold-starts Dolt in 30-45s, and the
// runner SIGKILLs it at gc's deadline — so gc's own clock demoted a long-lived
// handle for the process, and so did the lifecycle slot the controller's
// health loop holds while it recovers the same zombie. Design v2 3.4 ends the
// lane only when bd has been asked to recover and could not.
//
// One row per outcome class. Every non-terminal row also proves the rung is
// held for a backoff rather than spent (the next open inside it spends no
// verb) and released after it (the open after it recovers).
func TestZombieLadderEndsTheLaneOnlyOnARecoverBdRefused(t *testing.T) {
	bdSaysNo := func() error {
		return ProviderReportedFailure(errors.New("provider-owned beads probe: Error: ping: invalid connection"))
	}
	silent := proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeAcceptedNoGreeting}

	for _, tc := range []struct {
		name string
		// recoverFails is the recover's failure. It may end ctx first, which
		// is how gc's own clock or cancellation reaches the ladder.
		recoverFails func(ctx *expiringContext, cancel context.CancelFunc, scopeRoot string) error
		// useCancel runs the row under a real cancelable context instead of
		// the expiring one.
		useCancel bool
		want      ProxiedVerdict
		terminal  bool
	}{
		{
			name: "gc's own deadline SIGKILLed the recover mid-cold-start",
			recoverFails: func(ctx *expiringContext, _ context.CancelFunc, _ string) error {
				ctx.expire(context.DeadlineExceeded)
				return fmt.Errorf("provider-owned beads recover: %w", context.DeadlineExceeded)
			},
			want: ProxiedVerdictBudgetExhausted,
		},
		{
			name:      "gc's own cancellation ended the recover",
			useCancel: true,
			recoverFails: func(_ *expiringContext, cancel context.CancelFunc, _ string) error {
				cancel()
				return fmt.Errorf("provider-owned beads recover: %w", context.Canceled)
			},
			want: ProxiedVerdictBudgetExhausted,
		},
		{
			name: "the lifecycle slot the health loop's recover of the same zombie holds ran out",
			recoverFails: func(_ *expiringContext, _ context.CancelFunc, scopeRoot string) error {
				return fmt.Errorf("waiting for provider lifecycle slot for %q: %w", scopeRoot, context.DeadlineExceeded)
			},
			want: ProxiedVerdictBudgetExhausted,
		},
		{
			name: "the recover failed before bd could answer (a signal, an environment gc could not build)",
			recoverFails: func(*expiringContext, context.CancelFunc, string) error {
				return errors.New("provider-owned beads recover: signal: killed")
			},
			want: ProxiedVerdictBackendUnreachable,
		},
		{
			name: "bd's refusal raced gc's own clock",
			recoverFails: func(ctx *expiringContext, _ context.CancelFunc, _ string) error {
				ctx.expire(context.DeadlineExceeded)
				return ProviderReportedFailure(errors.New("provider-owned beads recover: exit status 1"))
			},
			want: ProxiedVerdictBudgetExhausted,
		},
		{
			name: "bd ran the recover and refused: the one terminal outcome",
			recoverFails: func(*expiringContext, context.CancelFunc, string) error {
				return ProviderReportedFailure(errors.New("provider-owned beads recover: Error: ping: invalid connection"))
			},
			want:     ProxiedVerdictProxyZombie,
			terminal: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdmissionFixture(t, "-1")
			generation := proxyendpoint.NewPoolKey(f.record, "").Generation()
			expiring := newExpiringContext()
			var ctx context.Context = expiring
			cancel := context.CancelFunc(func() {})
			if tc.useCancel {
				ctx, cancel = context.WithCancel(context.Background())
				defer cancel()
			}

			recovered := false
			failRecover := true
			ops := &admissionOps{onPing: bdSaysNo}
			ops.onRecov = func() error {
				if failRecover {
					return tc.recoverFails(expiring, cancel, f.scopeRoot)
				}
				// `bd dolt stop` then `bd ping`: a new generation, which greets.
				f.writeRecord(6002, "55667788")
				recovered = true
				return nil
			}
			in := baseAdmissionInput(f, ops)
			in.Probe = func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
				if recovered {
					return proxyendpoint.ServedProbeForTest(pinnedCursors(), proxyendpoint.CursorReality{})
				}
				return silent
			}
			clock := time.Now()
			in.Recovered.now = func() time.Time { return clock }

			_, err := Admit(ctx, in)
			verdict, ok := ProxiedVerdictOf(err)
			if !ok || verdict.Verdict != tc.want {
				t.Fatalf("Admit = %v, want verdict %s", err, tc.want)
			}
			if verdict.Terminal() != tc.terminal {
				t.Fatalf("terminal = %v, want %v for %v: only a recover bd ran and refused may end the lane "+
					"for the handle, and gc's own clock, cancellation or contention never may", verdict.Terminal(), tc.terminal, err)
			}
			if pings, recovers := ops.counts(); pings != 1 || recovers != 1 {
				t.Fatalf("the ladder spent %d ping(s) and %d recover(s), want exactly 1 and 1", pings, recovers)
			}

			// A second open inside the window spends nothing, whichever way
			// the first ended: a terminal one because the rung is spent, a
			// non-terminal one because it is held for a backoff.
			_, err = Admit(context.Background(), in)
			verdict, ok = ProxiedVerdictOf(err)
			if !ok || verdict.Terminal() != tc.terminal {
				t.Fatalf("the second open = %v, want terminal = %v", err, tc.terminal)
			}
			if pings, recovers := ops.counts(); pings != 1 || recovers != 1 {
				t.Fatalf("the second open of the same generation spent more: %d ping(s), %d recover(s), want 1 and 1",
					pings, recovers)
			}
			if tc.terminal {
				return
			}
			if _, backing := in.Recovered.BackingOff(generation); !backing {
				t.Fatal("a recover that failed on gc's side left no backoff on its rung")
			}

			// Once the backoff runs out the rung may be spent again — here,
			// with gc's contention gone, and bd's recover works.
			clock = clock.Add(failedRecoverBackoff)
			failRecover = false
			pin, err := Admit(context.Background(), in)
			if err != nil {
				t.Fatalf("the open after the backoff = %v, want the recovered generation admitted", err)
			}
			if pin.PoolKey().PID != 6002 {
				t.Fatalf("admitted pid %d, want the generation the second recover produced (6002)", pin.PoolKey().PID)
			}
			if pings, recovers := ops.counts(); pings != 1 || recovers != 2 {
				t.Fatalf("the open after the backoff spent %d ping(s) and %d recover(s) in total, want 1 and 2: "+
					"bd already answered this generation's ping, and the recover rung gc's side failed is owed once more",
					pings, recovers)
			}
		})
	}
}

// sharedRootRig writes a rig scope whose sidecar names the CITY's proxy root —
// the shape `gc beads city migrate-proxied` leaves, one proxy and one Dolt
// child serving hq and every rig. The root_path is RELATIVE and climbs out of
// the rig through `..`, the way the migration writes it, so the shared-root
// test has to resolve it physically.
func sharedRootRig(t *testing.T, city *admissionFixture) string {
	t.Helper()
	rig := t.TempDir()
	beadsDir := filepath.Join(rig, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(beadsDir, city.root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"),
		[]byte(`{"backend":"dolt","dolt_mode":"proxied-server","dolt_database":"rig"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(proxyendpoint.SidecarPath(beadsDir),
		[]byte(`{"root_path":"`+rel+`","port":44561,"idle_timeout":-1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := proxyendpoint.ProviderRoot(rig)
	if err != nil || !samePhysicalDir(root, city.root) {
		t.Fatalf("the rig resolves proxy root %q (%v), want the city's %q: the fixture is not the shared-root shape",
			root, err, city.root)
	}
	return rig
}

func samePhysicalDir(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// TestZombieLadderLeavesTheSharedRootsRecoverToTheCity is round4 recheck M2.
//
// A rig sharing the city's proxy root reads the city's proxy.pid, so it has
// the city's generation, and the recover ledger is process-global and keyed on
// the generation. The rig's recover op is a ping alone (the script will not
// `bd dolt stop` the pair serving hq and every rig from a rig), yet it spent
// the generation's one recover: when the first ladder in the process was the
// rig's, the rig went terminal on its own useless ping, and the city's ladder
// found the rung spent and went terminal with no verb — `bd dolt stop` never
// ran and both handles were demoted for the process.
//
// Each row is the interleaving: the RIG reaches the zombie first, then the
// city.
func TestZombieLadderLeavesTheSharedRootsRecoverToTheCity(t *testing.T) {
	bdSaysNo := func() error {
		return ProviderReportedFailure(errors.New("provider-owned beads probe: Error: ping: invalid connection"))
	}
	silent := proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeAcceptedNoGreeting}

	type pair struct {
		city, rig       AdmissionInput
		cityOps, rigOps *admissionOps
		recovered       *bool
		generation      string
	}
	build := func(t *testing.T, cityRecover func(f *admissionFixture, recovered *bool) error) pair {
		t.Helper()
		f := newAdmissionFixture(t, "-1")
		rig := sharedRootRig(t, f)
		recovered := false
		probe := func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
			if recovered {
				return proxyendpoint.ServedProbeForTest(pinnedCursors(), proxyendpoint.CursorReality{})
			}
			return silent
		}
		cityOps := &admissionOps{
			onPing:  func() error { t.Error("the city pinged a generation bd has already answered"); return nil },
			onRecov: func() error { return cityRecover(f, &recovered) },
		}
		rigOps := &admissionOps{onPing: bdSaysNo, onRecov: func() error {
			// What the script does for this scope: `bd ping` alone, which
			// fails against the zombie. Reaching here at all is the defect.
			return ProviderReportedFailure(errors.New("provider-owned beads recover: Error: ping: invalid connection"))
		}}
		// ONE pair of ledgers, as in production: every scope's opener gets
		// the package-level sets.
		observed, recoveredSet := NewGenerationSet(), NewGenerationSet()

		city := baseAdmissionInput(f, cityOps)
		city.CityRoot = f.scopeRoot
		city.Probe = probe
		city.Observed, city.Recovered = observed, recoveredSet

		rigIn := baseAdmissionInput(f, rigOps)
		rigIn.ScopeRoot = rig
		rigIn.Database = "rig"
		rigIn.CityRoot = f.scopeRoot
		rigIn.Probe = probe
		rigIn.Observed, rigIn.Recovered = observed, recoveredSet

		return pair{
			city: city, rig: rigIn, cityOps: cityOps, rigOps: rigOps, recovered: &recovered,
			generation: proxyendpoint.NewPoolKey(f.record, "").Generation(),
		}
	}
	cityRecovers := func(f *admissionFixture, recovered *bool) error {
		// `bd dolt stop` then `bd ping`, from the city: a new generation,
		// which greets.
		f.writeRecord(6002, "55667788")
		*recovered = true
		return nil
	}

	t.Run("rig first, then the city's zombie still recovers", func(t *testing.T) {
		p := build(t, cityRecovers)

		for open := 1; open <= 2; open++ {
			_, err := Admit(context.Background(), p.rig)
			verdict, ok := ProxiedVerdictOf(err)
			if !ok || verdict.Terminal() {
				t.Fatalf("rig open %d = %v, want a non-terminal verdict: the rig cannot cycle the shared proxy, "+
					"so nothing it learned ends its lane", open, err)
			}
		}
		if pings, recovers := p.rigOps.counts(); pings != 1 || recovers != 0 {
			t.Fatalf("the rig spent %d ping(s) and %d recover(s), want 1 and 0: its recover is a ping bd has just "+
				"refused, and the rung is the city's", pings, recovers)
		}
		if p.city.Recovered.Has(p.generation) {
			t.Fatalf("the rig spent generation %s's recover rung; the city's `bd dolt stop` can now never run", p.generation)
		}

		pin, err := Admit(context.Background(), p.city)
		if err != nil {
			t.Fatalf("city Admit = %v after the rig's ladder: the city's own zombie was not recovered", err)
		}
		if pin.PoolKey().PID != 6002 {
			t.Fatalf("the city admitted pid %d, want the generation its recover produced (6002)", pin.PoolKey().PID)
		}
		if pings, recovers := p.cityOps.counts(); pings != 0 || recovers != 1 {
			t.Fatalf("the city spent %d ping(s) and %d recover(s), want 0 and 1: bd already answered this "+
				"generation's ping for the rig, and the recover is the city's to spend", pings, recovers)
		}

		// And the rig follows the city onto the recovered generation, for free.
		rigPin, err := Admit(context.Background(), p.rig)
		if err != nil {
			t.Fatalf("rig Admit after the city's recover = %v, want the recovered generation", err)
		}
		if rigPin.PoolKey().PID != 6002 {
			t.Fatalf("the rig admitted pid %d, want 6002", rigPin.PoolKey().PID)
		}
		if pings, recovers := p.rigOps.counts(); pings != 1 || recovers != 0 {
			t.Fatalf("the rig spent %d ping(s) and %d recover(s) in total, want 1 and 0", pings, recovers)
		}
	})

	t.Run("the rig turns terminal only once the city's recover was spent and did not fix it", func(t *testing.T) {
		p := build(t, func(*admissionFixture, *bool) error {
			// bd's recover ran and refused: the city's determinate end.
			return ProviderReportedFailure(errors.New("provider-owned beads recover: Error: ping: invalid connection"))
		})
		if _, err := Admit(context.Background(), p.rig); err == nil {
			t.Fatal("a silent proxy admitted the rig")
		}
		_, err := Admit(context.Background(), p.city)
		if verdict, ok := ProxiedVerdictOf(err); !ok || verdict.Verdict != ProxiedVerdictProxyZombie || !verdict.Terminal() {
			t.Fatalf("city Admit = %v, want the terminal proxy_zombie of a recover bd refused", err)
		}
		_, err = Admit(context.Background(), p.rig)
		if verdict, ok := ProxiedVerdictOf(err); !ok || verdict.Verdict != ProxiedVerdictProxyZombie || !verdict.Terminal() {
			t.Fatalf("rig Admit after the city's spent recover = %v, want a terminal proxy_zombie on the city's terms", err)
		}
		if pings, recovers := p.rigOps.counts(); pings != 1 || recovers != 0 {
			t.Fatalf("the rig spent %d ping(s) and %d recover(s), want 1 and 0", pings, recovers)
		}
	})

	t.Run("a city recover gc's side cut short leaves the rig non-terminal", func(t *testing.T) {
		p := build(t, func(*admissionFixture, *bool) error {
			return fmt.Errorf("provider-owned beads recover: %w", context.DeadlineExceeded)
		})
		_, _ = Admit(context.Background(), p.rig)
		if _, err := Admit(context.Background(), p.city); err == nil {
			t.Fatal("a silent proxy admitted the city")
		}
		_, err := Admit(context.Background(), p.rig)
		if verdict, ok := ProxiedVerdictOf(err); !ok || verdict.Terminal() {
			t.Fatalf("rig Admit while the city's recover rung is backing off = %v, want non-terminal", err)
		}
	})

	t.Run("control: a rig on its OWN root still spends its own recover", func(t *testing.T) {
		city := newAdmissionFixture(t, "-1")
		rig := newAdmissionFixture(t, "-1")
		ops := &admissionOps{onPing: bdSaysNo}
		in := baseAdmissionInput(rig, ops)
		in.CityRoot = city.scopeRoot
		in.Probe = func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult { return silent }
		if _, err := Admit(context.Background(), in); err == nil {
			t.Fatal("a silent proxy admitted")
		}
		if pings, recovers := ops.counts(); pings != 1 || recovers != 1 {
			t.Fatalf("a rig that owns its root spent %d ping(s) and %d recover(s), want 1 and 1: only a SHARED "+
				"root's recover is the city's", pings, recovers)
		}
	})
}

// TestZombieLadderHoldsTheBackoffWhenTheRecordIsUnreadableAfterAFailedPing is
// council pr2 E-S6.
//
// Observed.Add writes a success-shaped entry for the generation BEFORE the
// ping. When the ping failed and the record read right after it failed too
// (EMFILE, EIO, a torn read), sameGeneration reported "moved" and the arm
// re-admitted without Backoff — so the entry stayed success-shaped while the
// generation was in fact still current, and the next silent open found the
// ping "spent", no backoff, and recovered: a `bd dolt stop` with no successful
// ping on this generation.
func TestZombieLadderHoldsTheBackoffWhenTheRecordIsUnreadableAfterAFailedPing(t *testing.T) {
	f := newAdmissionFixture(t, "-1")
	pidPath := proxyendpoint.PIDPath(f.root)
	calls := 0
	ops := &admissionOps{onPing: func() error {
		calls++
		switch calls {
		case 1:
			// The read right after this failed ping cannot see the record.
			if err := os.Rename(pidPath, pidPath+".unreadable"); err != nil {
				t.Errorf("hide the record: %v", err)
			}
		case 2:
			// ...and it is back, naming the SAME generation.
			if err := os.Rename(pidPath+".unreadable", pidPath); err != nil {
				t.Errorf("restore the record: %v", err)
			}
		}
		return errors.New("provider op timed out waiting on the lifecycle semaphore")
	}}
	in := baseAdmissionInput(f, ops)
	in.Probe = func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
		return proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeAcceptedNoGreeting}
	}
	clock := time.Now()
	in.Observed.now = func() time.Time { return clock }

	if _, err := Admit(context.Background(), in); err == nil {
		t.Fatal("a silent proxy admitted")
	}
	if _, recovers := ops.counts(); recovers != 0 {
		t.Fatalf("the first open recovered %d time(s) on a failed ping", recovers)
	}
	pingsBefore, _ := ops.counts()

	// A second open inside the backoff, with the record readable and the
	// generation unchanged.
	if _, err := Admit(context.Background(), in); err == nil {
		t.Fatal("a silent proxy admitted")
	}
	if pings, recovers := ops.counts(); recovers != 0 || pings != pingsBefore {
		t.Fatalf("the open after a failed ping and an unreadable record spent %d more ping(s) and %d recover(s), "+
			"want 0 and 0: a `bd dolt stop` with no successful ping on this generation", pings-pingsBefore, recovers)
	}
}

// TestAbsentRecordPingIsOncePerIncidentNotOncePerProcess is council A-F6.
//
// escalateWithPing has no generation for an absent record, so it keys the rung
// on the scope. The ledger was never-expiring and the key was never released,
// so the FIRST open of a scope whose proxy is stopped spent the ping and every
// later open in that process returned proxy_gone having spent nothing —
// whether the first ping had succeeded or failed.
//
// For a controller that is the difference between the P2-15 acceptance row
// ("one `bd dolt stop` costs one ping and a re-pin") and "the second stop in
// this process is never recovered": the guard leaves an absent record
// undecided, the reads fail transiently, the reopen hook re-admits, and
// admission declines to ask.
func TestAbsentRecordPingIsOncePerIncidentNotOncePerProcess(t *testing.T) {
	f := newAdmissionFixture(t, "-1")
	observed := NewGenerationSet()
	clock := time.Now()
	observed.now = func() time.Time { return clock }
	probes := 0

	var pingErr error
	ops := &admissionOps{onPing: func() error {
		if pingErr != nil {
			return pingErr
		}
		// bd restarted its proxy.
		f.writeRecord(6100, "11223344")
		return nil
	}}

	in := baseAdmissionInput(f, ops)
	in.Observed = observed
	in.Probe = servedProbe(pinnedCursors(), &probes)

	// Incident 1, and the ping fails on something of gc's own.
	f.removeRecord()
	pingErr = errors.New("provider op timed out waiting on the lifecycle semaphore")
	if _, err := Admit(context.Background(), in); err == nil {
		t.Fatal("Admit succeeded with no proxy record and a failing ping")
	}
	if pings, _ := ops.counts(); pings != 1 {
		t.Fatalf("the first open spent %d ping(s), want 1", pings)
	}

	// Inside the failed ping's backoff the next open does NOT re-fork it
	// (council pr2 D-F9)...
	pingErr = nil
	if _, err := Admit(context.Background(), in); err == nil {
		t.Fatal("an open inside the failed ping's backoff admitted with no proxy record")
	}
	if pings, _ := ops.counts(); pings != 1 {
		t.Fatalf("an open inside the backoff spent %d ping(s) in total, want 1: every open re-forked the failing ping", pings)
	}

	// ...and once it runs out, the next open asks again: the failed ping
	// bought nothing, and it must not poison the scope for the whole TTL.
	clock = clock.Add(failedPingBackoff)
	pin, err := Admit(context.Background(), in)
	if err != nil {
		t.Fatalf("the second open could not re-ask: %v", err)
	}
	if pin.PoolKey().PID != 6100 {
		t.Fatalf("admitted pid %d, want the proxy the ping produced (6100)", pin.PoolKey().PID)
	}
	if pings, _ := ops.counts(); pings != 2 {
		t.Fatalf("the second open spent %d ping(s) in total, want 2: a ping that failed for gc's own "+
			"reason must not poison the scope", pings)
	}

	// Incident 2: the operator stops the proxy again, in the same process.
	// This is the case that could never be recovered.
	f.removeRecord()
	if _, err := Admit(context.Background(), in); err != nil {
		t.Fatalf("the second incident was not recovered: %v", err)
	}
	if pings, _ := ops.counts(); pings != 3 {
		t.Fatalf("the second `bd dolt stop` in this process spent %d ping(s) in total, want 3; "+
			"the scope was poisoned for the process lifetime", pings)
	}
}

// TestDrainReProbesTheDataPort is council A-F7.
//
// ECONNREFUSED on a live, same-generation record is the signature of a proxy on
// its way DOWN and of one on its way UP: bd's supervisor writes the record at
// start, binds its listener afterwards, and its Dolt child can cold-start for
// tens of seconds. The drain loop only re-read the RECORD, and in the starting
// case the generation never moves — so a long-lived open that caught that
// window ran the full 60s admissionDrainCeiling and then refused, and a
// controller boot fell to BdStore for the whole process over a proxy that was
// about to answer.
func TestDrainReProbesTheDataPort(t *testing.T) {
	f := newAdmissionFixture(t, "-1")
	ops := &admissionOps{}

	// A fake clock the drain's own Sleep advances, so the 60s ceiling is real
	// to the code and instant to the test.
	now := time.Now()
	probes := 0

	in := baseAdmissionInput(f, ops)
	in.LongLived = true
	in.Now = func() time.Time { return now }
	in.Sleep = func(_ context.Context, d time.Duration) error {
		now = now.Add(d)
		return nil
	}
	// The proxy is STARTING: the record is live and unchanged throughout, the
	// port refuses, and then the listener binds.
	in.Probe = func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
		probes++
		if probes <= 2 {
			return proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeRefused}
		}
		return proxyendpoint.ServedProbeForTest(pinnedCursors(), proxyendpoint.CursorReality{})
	}

	start := now
	pin, err := Admit(context.Background(), in)
	if err != nil {
		t.Fatalf("Admit across a starting proxy: %v", err)
	}
	if !pin.Admitted() {
		t.Fatal("Admit returned no error and no pin")
	}
	waited := now.Sub(start)
	if waited >= admissionDrainCeiling {
		t.Fatalf("the drain waited %s, i.e. the whole %s ceiling, for a proxy that answered: "+
			"the loop is still watching only the record", waited, admissionDrainCeiling)
	}
	// One re-probe per admissionDrainProbesEvery polls, so the wait is bounded
	// by the re-probe cadence rather than by the ceiling.
	if want := time.Duration(admissionDrainProbesEvery) * admissionDrainPoll; waited > 2*want {
		t.Errorf("the drain waited %s for a port that answered, want at most two re-probe intervals (%s)",
			waited, 2*want)
	}
	if pings, recovers := ops.counts(); pings != 0 || recovers != 0 {
		t.Errorf("waiting out a starting proxy spent %d ping / %d recover, want 0/0", pings, recovers)
	}
}

// TestDrainStillHonoursTheCeilingForAPortThatNeverAnswers is the other side: a
// port that stays refused must still end at the ceiling, and must not buy a
// probe session on every 250ms poll on the way there.
func TestDrainStillHonoursTheCeilingForAPortThatNeverAnswers(t *testing.T) {
	f := newAdmissionFixture(t, "-1")
	ops := &admissionOps{}
	now := time.Now()
	probes := 0

	in := baseAdmissionInput(f, ops)
	in.LongLived = true
	in.Now = func() time.Time { return now }
	in.Sleep = func(_ context.Context, d time.Duration) error {
		now = now.Add(d)
		return nil
	}
	in.Probe = func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
		probes++
		return proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeRefused}
	}

	_, err := Admit(context.Background(), in)
	verdict, ok := ProxiedVerdictOf(err)
	if !ok || verdict.Verdict != ProxiedVerdictDraining {
		t.Fatalf("Admit = %v, want a draining verdict", err)
	}
	if verdict.Terminal() {
		t.Error("a drain that ran out of wall clock says nothing about the proxy, so it must be non-terminal")
	}
	// The ceiling is reached once per pass; admitOnce's own probe opens each
	// pass. Whatever the pass count, the drain must not have probed once per
	// poll: that would be admissionDrainCeiling/admissionDrainPoll = 240
	// accepted connections per pass on a proxy bd's idle watcher is counting.
	perPass := int(admissionDrainCeiling/admissionDrainPoll)/admissionDrainProbesEvery + 1
	if probes > 4*perPass {
		t.Fatalf("the drain ran %d probe sessions, want at most %d per pass: re-probing on every poll "+
			"costs bd an accepted connection every 250ms for a minute", probes, perPass)
	}
}

// TestAdmitRefusesAnUncheckedCursorReality is council pr2 D-F11.
//
// CursorReality's zero value says "nothing was missing", which made the A-F2
// gate fail OPEN: a Session that filled the cursors and forgot the reality —
// cmd/gc's own test stubs built exactly that shape and passed — compared the
// raw ignored cursor, the comparison A-F2 exists to remove. Undetermined is
// never a pass: an unevaluated reality is refused, non-terminally, and a
// checked one with the pinned cursors still admits.
func TestAdmitRefusesAnUncheckedCursorReality(t *testing.T) {
	t.Run("a zero-value reality is refused, non-terminally", func(t *testing.T) {
		f := newAdmissionFixture(t, "-1")
		in := baseAdmissionInput(f, &admissionOps{})
		in.Probe = func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
			return proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeServed, Cursors: pinnedCursors()}
		}
		pin, err := Admit(context.Background(), in)
		if pin.Admitted() {
			t.Fatal("a served probe whose reality nobody evaluated was admitted")
		}
		verdict, typed := ProxiedVerdictOf(err)
		if !typed || verdict.Verdict != ProxiedVerdictSchemaUnverified {
			t.Fatalf("err = %v, want the schema_unverified verdict", err)
		}
		if verdict.Terminal() {
			t.Error("an unevaluated reality is a fact about the session, not the database: it must not latch")
		}
	})

	t.Run("a checked reality with the pinned cursors admits", func(t *testing.T) {
		f := newAdmissionFixture(t, "-1")
		in := baseAdmissionInput(f, &admissionOps{})
		calls := 0
		in.Probe = servedProbe(pinnedCursors(), &calls)
		pin, err := Admit(context.Background(), in)
		if err != nil || !pin.Admitted() {
			t.Fatalf("Admit = (%+v, %v), want an admitted pin", pin, err)
		}
	})
}

// TestProxiedOpenUnmovedDeclinesRatherThanAgrees is council pr2 D-F3's unit
// half.
//
// Both "did not move" and "could not tell" are legitimate answers, and the whole
// value of the check turns on their not being the same answer. A probe that
// cannot read HEAD does NOT degrade to "": it fails its session, which
// classifies ProbeUnknown and admits nothing (readMainCursorAndHead;
// TestProbeSessionReadsHeadOnItsFirstStatement's "a HEAD the server cannot
// answer" row). What does reach this function with an empty head is a
// memoized pin (Pin.withoutHead) and, in principle, a SQL NULL hash — so an
// empty-means-equal comparison would report those as clean opens and the check
// would quietly stop existing. (An earlier version of this doc said the probe
// degrades to "" on a failed read; that was the stopped lane's design, which
// the D-F3 round removed — council pr2 E-I4.)
func TestProxiedOpenUnmovedDeclinesRatherThanAgrees(t *testing.T) {
	pinAt := func(head string) Pin {
		return Pin{admitted: true, database: "beads", head: head, cursors: pinnedCursors()}
	}
	observed := func(head string) proxyendpoint.PostOpenReport {
		return proxyendpoint.PostOpenReport{Head: head, IgnoredCursorTable: true}
	}

	t.Run("an unchanged head admits", func(t *testing.T) {
		if err := ProxiedOpenUnmoved(pinAt("abc123"), observed("abc123")); err != nil {
			t.Fatalf("a healthy open was refused: %v", err)
		}
	})

	t.Run("a moved head is the head_moved verdict", func(t *testing.T) {
		err := ProxiedOpenUnmoved(pinAt("abc123"), observed("def456"))
		verdict, typed := ProxiedVerdictOf(err)
		if !typed {
			t.Fatalf("err = %v, want a typed verdict", err)
		}
		if verdict.Verdict != ProxiedVerdictHeadMoved {
			t.Fatalf("verdict = %q, want %q", verdict.Verdict, ProxiedVerdictHeadMoved)
		}
		if verdict.Terminal() {
			t.Error("head_moved must be non-terminal: gc cannot attribute the commit to its own open")
		}
	})

	for _, tc := range []struct{ name, before, after string }{
		{"the probe could not read a head", "", "def456"},
		{"the re-read produced nothing", "abc123", ""},
		{"neither side was observed", "", ""},
	} {
		t.Run(tc.name+" concludes nothing", func(t *testing.T) {
			if err := ProxiedOpenUnmoved(pinAt(tc.before), observed(tc.after)); err != nil {
				t.Fatalf("an unobserved hash was read as evidence: %v", err)
			}
		})
	}
}

// TestProxiedOpenUnmovedReadsTheIgnoredPlane is council pr2 E-S4.
//
// The dolt_ignore'd plane is never committed, so HEAD cannot see a write to it.
// The post-open observation reads the plane's cursor table and the library's
// sentinel reality in the same statement as HEAD, and the verdict fires when the
// effective ignored cursor they imply is not the admitted one — with HEAD
// unmoved in every row, which is the point: before E-S4 each of these was a
// clean open.
func TestProxiedOpenUnmovedReadsTheIgnoredPlane(t *testing.T) {
	pin := Pin{admitted: true, database: "beads", head: "abc123", cursors: pinnedCursors()}

	for _, tc := range []struct {
		name     string
		observed proxyendpoint.PostOpenReport
		want     int
	}{
		{
			name: "a sentinel table absent after the open floors the lane at 0",
			observed: proxyendpoint.PostOpenReport{Head: "abc123", IgnoredCursorTable: true, Reality: proxyendpoint.CursorReality{
				Limited: true, Floor: proxyendpoint.IgnoredSentinelTableFloor, Missing: "wisps",
			}},
			want: proxyendpoint.IgnoredSentinelTableFloor,
		},
		{
			name: "the sentinel column absent after the open floors the lane at 11",
			observed: proxyendpoint.PostOpenReport{Head: "abc123", IgnoredCursorTable: true, Reality: proxyendpoint.CursorReality{
				Limited: true, Floor: proxyendpoint.IgnoredSentinelColumnFloor, Missing: "leases.granted_node",
			}},
			want: proxyendpoint.IgnoredSentinelColumnFloor,
		},
		{
			name:     "a vanished cursor table is the cursor the library reads as 0",
			observed: proxyendpoint.PostOpenReport{Head: "abc123"},
			want:     0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ProxiedOpenUnmoved(pin, tc.observed)
			verdict, typed := ProxiedVerdictOf(err)
			if !typed || verdict.Verdict != ProxiedVerdictHeadMoved {
				t.Fatalf("err = %v, want the head_moved verdict with HEAD unmoved", err)
			}
			if verdict.Terminal() {
				t.Error("the ignored-plane arm must be non-terminal, like the HEAD arm it shares a verdict with")
			}
			for _, want := range []string{"ignored plane", fmt.Sprintf("cursor at %d", tc.want), fmt.Sprint(SchemaCursorIgnored)} {
				if !strings.Contains(verdict.Detail, want) {
					t.Errorf("the verdict detail lacks %q: %s", want, verdict.Detail)
				}
			}
		})
	}

	t.Run("a healthy plane with an unmoved head admits", func(t *testing.T) {
		if err := ProxiedOpenUnmoved(pin, proxyendpoint.PostOpenReport{Head: "abc123", IgnoredCursorTable: true}); err != nil {
			t.Fatalf("a healthy open was refused: %v", err)
		}
	})
}

// TestAdmitCarriesTheProbesHeadButNotTheMemos is the
// other end of the same finding.
//
// The pin's head is the pre-open half of a comparison the opener completes. It
// must survive a fresh admission — otherwise the opener has nothing to compare
// and the check is inert — and it must NOT survive the memo, because a hash up
// to proxiedPinMemoTTL old is a statement about some earlier open, and any bd
// client may have committed since. Reusing it would make another process's
// ordinary write read as gc's own, which is the one way a belt-and-braces check
// can do damage.
func TestAdmitCarriesTheProbesHeadButNotTheMemos(t *testing.T) {
	f := newAdmissionFixture(t, "-1")
	ForgetProxiedPin(f.scopeRoot, "beads")
	t.Cleanup(func() { ForgetProxiedPin(f.scopeRoot, "beads") })

	probes := 0
	in := baseAdmissionInput(f, &admissionOps{})
	in.SkipMemo = false
	in.Probe = func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
		probes++
		served := proxyendpoint.ServedProbeForTest(pinnedCursors(), proxyendpoint.CursorReality{})
		served.Head = "head0000"
		return served
	}

	fresh, err := Admit(context.Background(), in)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if fresh.Head() != "head0000" {
		t.Fatalf("a freshly probed pin carries head %q, want the probe's: the opener has nothing to compare", fresh.Head())
	}

	memoized, err := Admit(context.Background(), in)
	if err != nil {
		t.Fatalf("Admit (memoized): %v", err)
	}
	if probes != 1 {
		t.Fatalf("the second open probed again (%d sessions); this row must exercise the MEMO", probes)
	}
	if memoized.Head() != "" {
		t.Fatalf("a memoized pin carries head %q; comparing a post-open re-read against a hash up to the "+
			"memo TTL old would accuse another bd client's write of being gc's", memoized.Head())
	}
}

// TestAdmitProbeOnceSpendsOneSessionAndNeverWaits is council pr2 D-F5's
// admission half: the guard recovery's shape is one pass, at most one probe
// session and no sleeps, whatever the endpoint says. The rows without
// ProbeOnce are the control — they are what a recovery tick used to cost, on a
// virtual clock, and they are why the cap matters.
func TestAdmitProbeOnceSpendsOneSessionAndNeverWaits(t *testing.T) {
	for _, tc := range []struct {
		name        string
		outcome     proxyendpoint.ProbeOutcome
		probeOnce   bool
		wantProbes  int
		wantSleeps  int
		wantVerdict ProxiedVerdict
	}{
		{
			name: "a silent proxy, capped", outcome: proxyendpoint.ProbeAcceptedNoGreeting, probeOnce: true,
			wantProbes: 1, wantVerdict: ProxiedVerdictBackendUnreachable,
		},
		{
			name: "a refusing port, capped", outcome: proxyendpoint.ProbeRefused, probeOnce: true,
			wantProbes: 1, wantVerdict: ProxiedVerdictDraining,
		},
		{
			name: "a served database, capped", outcome: proxyendpoint.ProbeServed, probeOnce: true,
			wantProbes: 1,
		},
		// The controls: the same endpoints on the ordinary long-lived shape.
		{
			name: "a silent proxy, uncapped", outcome: proxyendpoint.ProbeAcceptedNoGreeting,
			wantProbes: admissionNoGreetingAttempts, wantSleeps: admissionNoGreetingAttempts - 1,
			wantVerdict: ProxiedVerdictBackendUnreachable,
		},
		{
			name: "a refusing port, uncapped", outcome: proxyendpoint.ProbeRefused,
			wantProbes:  1 + int(admissionDrainCeiling/admissionDrainPoll)/admissionDrainProbesEvery,
			wantSleeps:  int(admissionDrainCeiling / admissionDrainPoll),
			wantVerdict: ProxiedVerdictDraining,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdmissionFixture(t, "-1")
			in := baseAdmissionInput(f, nil)
			in.Ops = nil // the recovery holds no provider ops
			in.LongLived = true
			in.ProbeOnce = tc.probeOnce
			clock := time.Unix(1_700_000_000, 0)
			probes, sleeps := 0, 0
			in.Now = func() time.Time { return clock }
			in.Sleep = func(_ context.Context, d time.Duration) error {
				sleeps++
				clock = clock.Add(d)
				return nil
			}
			in.Probe = func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
				probes++
				// Served rows need a checked reality (council pr2 D-F11); for
				// every other outcome the reality is meaningless.
				result := proxyendpoint.ServedProbeForTest(pinnedCursors(), proxyendpoint.CursorReality{})
				result.Outcome = tc.outcome
				return result
			}

			_, err := Admit(context.Background(), in)
			if probes != tc.wantProbes || sleeps != tc.wantSleeps {
				t.Fatalf("admission spent %d probe session(s) and %d sleep(s), want %d and %d",
					probes, sleeps, tc.wantProbes, tc.wantSleeps)
			}
			if tc.wantVerdict == ProxiedVerdictNone {
				if err != nil {
					t.Fatalf("a served database was refused: %v", err)
				}
				return
			}
			verdict, typed := ProxiedVerdictOf(err)
			if !typed || verdict.Verdict != tc.wantVerdict || verdict.Terminal() {
				t.Fatalf("err = %v, want the non-terminal %s verdict the next tick can retry", err, tc.wantVerdict)
			}
		})
	}
}

// TestDrainIsNotEndedByAnIndeterminateProbe is council pr2 D-F10.
//
// A-F7's re-probe ended the drain on ANY outcome other than refused, including
// ProbeUnknown from the probe's own two-second clock — which PR1's contract says
// is never a conclusion about the proxy, and which is exactly what a probe
// session returns on a loaded box. So a genuinely draining proxy stopped being
// waited out at the first indeterminate re-probe (poll 8, ~2s), the pass re-ran,
// probed again, met the same indeterminate answer and returned a non-terminal
// budget_exhausted: the controller demoted over a two-second shutdown on
// precisely the box the long-lived drain was written for.
//
// Here bd replaces the draining generation at 5s while every re-probe of the
// OLD one comes back indeterminate. The drain must keep waiting through them,
// see the record move, and admit the new generation.
func TestDrainIsNotEndedByAnIndeterminateProbe(t *testing.T) {
	f := newAdmissionFixture(t, "-1")
	ops := &admissionOps{}
	now := time.Now()
	start := now
	replaced := false
	probes := 0

	in := baseAdmissionInput(f, ops)
	in.LongLived = true
	in.Now = func() time.Time { return now }
	in.Sleep = func(_ context.Context, d time.Duration) error {
		now = now.Add(d)
		if !replaced && now.Sub(start) >= 5*time.Second {
			f.writeRecord(6002, "99887766") // bd's replacement proxy
			replaced = true
		}
		return nil
	}
	in.Probe = func(_ context.Context, ep proxyendpoint.Endpoint, _ string) proxyendpoint.ProbeResult {
		probes++
		switch {
		case ep.Record.PID == 6002:
			return proxyendpoint.ServedProbeForTest(pinnedCursors(), proxyendpoint.CursorReality{})
		case probes == 1:
			return proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeRefused}
		default:
			// A loaded box: the probe's own session budget ran out.
			return proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeUnknown, Err: context.DeadlineExceeded}
		}
	}

	pin, err := Admit(context.Background(), in)
	if err != nil {
		t.Fatalf("Admit across a draining proxy with indeterminate re-probes: %v", err)
	}
	if pin.PoolKey().PID != 6002 {
		t.Fatalf("admitted pid %d, want bd's replacement proxy 6002", pin.PoolKey().PID)
	}
	if pings, recovers := ops.counts(); pings != 0 || recovers != 0 {
		t.Errorf("waiting out a drain spent %d ping / %d recover, want 0/0", pings, recovers)
	}

	// The control: a DETERMINATE changed answer still ends the drain, which is
	// A-F7's starting-proxy fix and must survive this one.
	t.Run("a served re-probe still ends the drain", func(t *testing.T) {
		f := newAdmissionFixture(t, "-1")
		now := time.Now()
		probes := 0
		in := baseAdmissionInput(f, &admissionOps{})
		in.LongLived = true
		in.Now = func() time.Time { return now }
		in.Sleep = func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }
		in.Probe = func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
			probes++
			if probes == 1 {
				return proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeRefused}
			}
			return proxyendpoint.ServedProbeForTest(pinnedCursors(), proxyendpoint.CursorReality{})
		}
		if _, err := Admit(context.Background(), in); err != nil {
			t.Fatalf("a proxy that came up during the drain was not admitted: %v", err)
		}
		if probes != 3 {
			t.Fatalf("the drain spent %d probe(s), want 3 (refused, served re-probe, served re-admission)", probes)
		}
	})
}
