package citysource

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/pkg/citytransport"
)

// multiLog serves several cities so one migration run exercises every class.
type multiLog struct {
	logs map[string]*fakeLog
	errs map[string]error
}

func (m *multiLog) Head(city string) (uint64, error) {
	if err := m.errs[city]; err != nil {
		return 0, err
	}
	l, ok := m.logs[city]
	if !ok {
		return 0, nil
	}
	return l.Head(city)
}

func (m *multiLog) OfferAt(city string, seq uint64) (citytransport.Offer, bool, error) {
	if err := m.errs[city]; err != nil {
		return citytransport.Offer{}, false, err
	}
	l, ok := m.logs[city]
	if !ok {
		return citytransport.Offer{}, false, nil
	}
	return l.OfferAt(city, seq)
}

// migrationCfg enrolls every city in the given set and refuses to invent an
// identity for any other.
func migrationCfg(enrolled ...string) MigrationConfig {
	set := map[string]bool{}
	for _, c := range enrolled {
		set[c] = true
	}
	return MigrationConfig{
		Now: testNow,
		SourceIDFor: func(city string) string {
			if !set[city] {
				return ""
			}
			return "src_" + city
		},
	}
}

// AC03: every class is counted explicitly, and the happy case backfills a proven
// sequence.
func TestMigrationClassifiesEveryLegacyCursor(t *testing.T) {
	proven := newFakeLog(50, "orig")
	short := newFakeLog(10, "orig")
	holed := newFakeLog(30, "orig")
	delete(holed.events, 12) // the cursor's own record is gone

	m := &multiLog{
		logs: map[string]*fakeLog{
			"proven": proven,
			"short":  short,
			"holed":  holed,
			"fresh":  newFakeLog(3, "orig"),
			"unread": newFakeLog(5, "orig"),
		},
		errs: map[string]error{"unread": errors.New("EIO")},
	}
	cfg := migrationCfg("proven", "short", "holed", "fresh", "unread")

	rep, err := Migrate(m, cfg, MigrationRequest{LegacyCursors: map[string]uint64{
		"proven": 42, // provable -> mapped
		"short":  25, // cursor beyond head -> conflicted
		"holed":  12, // within range but record absent -> quarantined
		"fresh":  0,  // never acked -> mapped clean
		"unread": 7,  // log unreadable -> unknown
	}})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	byCity := map[string]MigrationEntry{}
	for _, e := range rep.Entries {
		byCity[e.City] = e
	}
	want := map[string]string{
		"proven": ClassMapped, "short": ClassConflicted, "holed": ClassQuarantined,
		"fresh": ClassMapped, "unread": ClassUnknown,
	}
	for city, class := range want {
		if got := byCity[city].Class; got != class {
			t.Errorf("%s classified %q, want %q (detail: %s)", city, got, class, byCity[city].Detail)
		}
	}
	if rep.Mapped != 2 || rep.Conflicted != 1 || rep.Quarantined != 1 || rep.Unknown != 1 {
		t.Fatalf("counts must be explicit: %+v", rep)
	}

	// Happy: the proven sequence backfills with a real anchor.
	st := byCity["proven"].State
	if st == nil || st.Watermark != 42 || st.AnchorSeq != 42 || st.AnchorHash == "" {
		t.Fatalf("a proven cursor must backfill with an anchor: %+v", st)
	}
	if st.Epoch != 1 {
		t.Fatalf("migration lands on epoch 1, got %d", st.Epoch)
	}
	// A migrated state must pass the very check that protects it, under a
	// producer bound to that city.
	provenEnv := newTestEnv(t, proven)
	provenEnv.enroll.Identity = Identity{SourceID: "src_proven", CityHash: "ph", City: "proven"}
	prod, err := NewProducer(provenEnv.enroll, m, func() time.Time { return testNow })
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	if _, err := prod.Observe(*st); err != nil {
		t.Fatalf("a migrated state must observe clean: %v", err)
	}

	// Nothing unprovable gets state.
	for _, city := range []string{"short", "holed", "unread"} {
		if byCity[city].State != nil {
			t.Fatalf("%s must get no state: %+v", city, byCity[city].State)
		}
	}
}

// AC03 edge: an ambiguous actor stays unknown. The legacy source records a
// caller-supplied free-form actor with no principal binding, so there is nothing
// to promote and the migration must not invent an uploader.
func TestMigrationNeverFabricatesAnActor(t *testing.T) {
	m := &multiLog{logs: map[string]*fakeLog{"alpha": newFakeLog(10, "orig")}}
	cfg := migrationCfg("alpha")

	rep, err := Migrate(m, cfg, MigrationRequest{LegacyCursors: map[string]uint64{"alpha": 5}})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if rep.Entries[0].ActorResolution != ActorUnknown {
		t.Fatalf("with no verified binding the actor must remain %q, got %q", ActorUnknown, rep.Entries[0].ActorResolution)
	}

	// The report must carry no fabricated uploader anywhere in its bytes.
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, banned := range []string{"uploader_principal", "actor_hash", "\"actor\":"} {
		if strings.Contains(string(raw), banned) {
			t.Fatalf("report leaks a fabricated identity field %q: %s", banned, raw)
		}
	}

	// A caller that HAS verified a principal out of band may record it — and
	// that is the only way it ever appears.
	rep2, err := Migrate(m, cfg, MigrationRequest{
		LegacyCursors:    map[string]uint64{"alpha": 5},
		VerifiedUploader: "principal:svc-city-alpha",
	})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if rep2.Entries[0].ActorResolution != "principal:svc-city-alpha" {
		t.Fatalf("a verified principal must be recorded, got %q", rep2.Entries[0].ActorResolution)
	}
}

// AC03 verification method: the report is signed, and tampering with any count
// or entry breaks it.
func TestMigrationReportIsSignedAndTamperEvident(t *testing.T) {
	m := &multiLog{logs: map[string]*fakeLog{
		"alpha": newFakeLog(10, "orig"),
		"beta":  newFakeLog(4, "orig"),
	}}
	env := newTestEnv(t, newFakeLog(1, "x"))

	rep, err := Migrate(m, migrationCfg("alpha", "beta"), MigrationRequest{LegacyCursors: map[string]uint64{"alpha": 5, "beta": 9}})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	signed, err := SignMigrationReport(rep, "contract-governance-v1", env.priv)
	if err != nil {
		t.Fatalf("SignMigrationReport: %v", err)
	}
	if signed.Digest == "" || len(signed.Signature) == 0 {
		t.Fatal("a signed report needs both a digest and a signature")
	}
	if err := VerifyMigrationReport(signed, env.enroll.TrustedKeys); err != nil {
		t.Fatalf("a freshly signed report must verify: %v", err)
	}

	t.Run("tampered count", func(t *testing.T) {
		bad := signed
		bad.Conflicted = 0
		if err := VerifyMigrationReport(bad, env.enroll.TrustedKeys); !errors.Is(err, ErrPolicyMismatch) {
			t.Fatalf("want a digest mismatch, got %v", err)
		}
	})

	t.Run("tampered entry", func(t *testing.T) {
		bad := signed
		bad.Entries = append([]MigrationEntry(nil), signed.Entries...)
		bad.Entries[0].LegacySeq = 999
		if err := VerifyMigrationReport(bad, env.enroll.TrustedKeys); !errors.Is(err, ErrPolicyMismatch) {
			t.Fatalf("want a digest mismatch, got %v", err)
		}
	})

	t.Run("unknown key", func(t *testing.T) {
		otherPub, _, _ := ed25519.GenerateKey(nil)
		err := VerifyMigrationReport(signed, map[string]ed25519.PublicKey{"contract-governance-v1": otherPub})
		if !errors.Is(err, ErrPolicyUnknown) {
			t.Fatalf("want ErrPolicyUnknown, got %v", err)
		}
	})
}

// The report must be deterministic: the same inputs produce the same digest, or
// it is not evidence.
func TestMigrationReportIsDeterministic(t *testing.T) {
	m := &multiLog{logs: map[string]*fakeLog{
		"gamma": newFakeLog(10, "orig"), "alpha": newFakeLog(10, "orig"), "beta": newFakeLog(10, "orig"),
	}}
	env := newTestEnv(t, newFakeLog(1, "x"))
	cfg := migrationCfg("alpha", "beta", "gamma")
	cursors := map[string]uint64{"gamma": 3, "alpha": 5, "beta": 7}

	a, err := Migrate(m, cfg, MigrationRequest{LegacyCursors: cursors})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	b, err := Migrate(m, cfg, MigrationRequest{LegacyCursors: cursors})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	sa, _ := SignMigrationReport(a, "k", env.priv)
	sb, _ := SignMigrationReport(b, "k", env.priv)
	if sa.Digest != sb.Digest {
		t.Fatalf("report digest is not deterministic: %s vs %s", sa.Digest, sb.Digest)
	}
	// Map iteration order must not leak into the report.
	for i := range sa.Entries {
		if sa.Entries[i].City != sb.Entries[i].City {
			t.Fatalf("entry order is not stable at %d: %s vs %s", i, sa.Entries[i].City, sb.Entries[i].City)
		}
	}
	if sa.Entries[0].City != "alpha" {
		t.Fatalf("entries must be sorted by city, got %s first", sa.Entries[0].City)
	}
}
