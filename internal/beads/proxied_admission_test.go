package beads

import (
	"context"
	"encoding/json"
	"errors"
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
		return proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeServed, Cursors: cursors}
	}
}

// servedProbeWithReality answers with a served endpoint whose ignored-lane
// cursor the live schema contradicts. It is the shape council A-F2 is about: the
// number ON DISK is the pinned one, and the number the linked library acts on is
// not.
func servedProbeWithReality(cursors proxyendpoint.Cursors, reality proxyendpoint.CursorReality, calls *int) func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
	return func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
		*calls++
		return proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeServed, Cursors: cursors, Reality: reality}
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
			return proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeServed, Cursors: pinnedCursors()}
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

	// Release is the other half: a rung that bought nothing does not count.
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
}

// TestZombieLadderDoesNotRecoverOnAFailedPing is the second half of A-F5.
//
// A provider ping can fail for reasons that are entirely gc's — the lifecycle
// semaphore, the op budget — and none of them is evidence that bd cannot make
// its proxy healthy. Cascading into `bd dolt stop` in the same pass spends the
// most destructive rung in the ladder on gc's own contention.
func TestZombieLadderDoesNotRecoverOnAFailedPing(t *testing.T) {
	f := newAdmissionFixture(t, "-1")
	ops := &admissionOps{onPing: func() error {
		return errors.New("provider op timed out waiting on the lifecycle semaphore")
	}}

	in := baseAdmissionInput(f, ops)
	in.Probe = func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
		return proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeAcceptedNoGreeting}
	}

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

	// The rung was released, so a later open — when the semaphore is free — may
	// ask bd again instead of finding the scope poisoned.
	ops.onPing = nil
	if _, err := Admit(context.Background(), in); err == nil {
		t.Fatal("the second admission admitted a proxy that still never greets")
	}
	if pings, _ := ops.counts(); pings != 2 {
		t.Fatalf("the second open spent %d ping(s) in total, want 2: a ping that failed for gc's own "+
			"reason must not count as the rung", pings)
	}
}
