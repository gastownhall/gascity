package cityneutral

import (
	"context"
	"errors"
	"testing"
	"time"
)

var t0 = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

func citySource() Source {
	return Source{SourceID: "gc-city-01", Kind: "city", Epoch: 1}
}

func sampleChain() CityChain {
	end := t0.Add(9 * time.Minute)
	return CityChain{
		Run: CityRun{
			RunID: "city-run-7", Status: "succeeded", Started: t0, Ended: &end,
			ProjectID: "proj-a", IssueID: "bd-1", Version: 3,
			Rig: "rig-alpha", Formula: "formula-bravo", City: "sf",
		},
		Sessions: []CitySession{{
			SessionID: "city-sess-1", Status: "succeeded", Started: t0, Ended: &end,
			Version: 2, Complete: true,
			Records: []CityRecord{
				{
					MessageID: "m1", Ordinal: 1, Role: "user", At: t0.Add(time.Minute),
					Author: &ContributorRef{Kind: "human", Ref: "u-42"}, Text: "kick it off",
				},
				{
					MessageID: "m2", Ordinal: 2, Role: "agent", At: t0.Add(2 * time.Minute),
					Author: &ContributorRef{Kind: "agent", Ref: "worker-9"}, Text: "on it",
				},
				{MessageID: "m3", Ordinal: 3, Role: "tool", At: t0.Add(3 * time.Minute), ContentRef: "blob-3"},
			},
		}},
	}
}

func newCityProducer(t *testing.T, f *Fake, mut func(*Mapper)) *Producer {
	t.Helper()
	m := Mapper{Source: citySource(), AllowRawContent: true}
	if mut != nil {
		mut(&m)
	}
	p, err := NewProducer(f, m, NewMemoryStore())
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	return p
}

// customUpload is a producer with no City in the picture: it builds the neutral
// bodies from its own domain and mints its own keys. It exists so the parity
// assertion compares City against a real second implementation rather than
// against the City adapter wearing a different hat.
func customUpload(t *testing.T, api API) (string, string) {
	t.Helper()
	ctx := context.Background()
	end := t0.Add(9 * time.Minute)
	run, err := api.UpsertRun(ctx, Upsert{
		SourceEntityID: "orch-run-7", SourceVersion: 3, Epoch: 1,
		Status: StatusSucceeded, Lifecycle: LifecyclePartial,
		StartedAt: t0, EndedAt: &end, ProjectID: "proj-a", IssueID: "bd-1",
		Coverage: CoverageKnown,
	}, "11111111-1111-5111-8111-111111111111")
	if err != nil {
		t.Fatalf("custom UpsertRun: %v", err)
	}
	sess, err := api.UpsertRunSession(ctx, run.ID, Upsert{
		SourceEntityID: "orch-sess-1", SourceVersion: 2, Epoch: 1,
		Status: StatusSucceeded, Lifecycle: LifecyclePartial,
		StartedAt: t0, EndedAt: &end, Coverage: CoverageKnown,
	}, "22222222-2222-5222-8222-222222222222")
	if err != nil {
		t.Fatalf("custom UpsertRunSession: %v", err)
	}
	ords := []uint64{1, 2, 3}
	bodies := []TranscriptRecordIngest{
		{
			SourceMessageID: "a1", SourceVersion: 1, Epoch: 1, Ordinal: &ords[0], Role: RoleUser,
			Author: &ContributorRef{Kind: "human", Ref: "u-42"}, OccurredAt: t0.Add(time.Minute),
			Text: "kick it off", Coverage: CoverageKnown,
		},
		{
			SourceMessageID: "a2", SourceVersion: 1, Epoch: 1, Ordinal: &ords[1], Role: RoleAssistant,
			Author: &ContributorRef{Kind: "agent", Ref: "worker-9"}, OccurredAt: t0.Add(2 * time.Minute),
			Text: "on it", Coverage: CoverageKnown,
		},
		{
			SourceMessageID: "a3", SourceVersion: 1, Epoch: 1, Ordinal: &ords[2], Role: RoleTool,
			OccurredAt: t0.Add(3 * time.Minute), ContentRef: "blob-3", Coverage: CoveragePartial,
		},
	}
	for i, b := range bodies {
		key := []string{
			"33333333-3333-5333-8333-333333333331",
			"33333333-3333-5333-8333-333333333332",
			"33333333-3333-5333-8333-333333333333",
		}[i]
		if _, err := api.CreateTranscriptRecord(ctx, sess.ID, b, key); err != nil {
			t.Fatalf("custom CreateTranscriptRecord %d: %v", i, err)
		}
	}
	if _, err := api.FinalizeSession(ctx, sess.ID, "44444444-4444-5444-8444-444444444444"); err != nil {
		t.Fatalf("custom FinalizeSession: %v", err)
	}
	return run.ID, sess.ID
}

type shape struct {
	runStatus     Status
	runLifecycle  Lifecycle
	runFinalized  bool
	sessStatus    Status
	sessLifecycle Lifecycle
	sessFinalized bool
	sessLinksRun  bool
	roles         []Role
	authors       []string
	ordinals      []uint64
	contentStatus []string
	transcript    []string
}

func readShape(t *testing.T, api API, runID, sessID string) shape {
	t.Helper()
	ctx := context.Background()
	run, err := api.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	sess, err := api.GetSession(ctx, sessID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	recs, err := api.ListTranscriptRecords(ctx, sessID)
	if err != nil {
		t.Fatalf("ListTranscriptRecords: %v", err)
	}
	items, err := api.GetSessionTranscript(ctx, sessID)
	if err != nil {
		t.Fatalf("GetSessionTranscript: %v", err)
	}
	s := shape{
		runStatus: run.Status, runLifecycle: run.Lifecycle, runFinalized: run.Finalized,
		sessStatus: sess.Status, sessLifecycle: sess.Lifecycle, sessFinalized: sess.Finalized,
		sessLinksRun: sess.RunID == run.ID,
	}
	for _, r := range recs {
		s.roles = append(s.roles, r.Role)
		author := ""
		if r.Author != nil {
			author = r.Author.Kind + ":" + r.Author.Ref
		}
		s.authors = append(s.authors, author)
		s.ordinals = append(s.ordinals, *r.Ordinal)
		s.contentStatus = append(s.contentStatus, r.ContentStatus)
	}
	for _, it := range items {
		s.transcript = append(s.transcript, string(it.Role)+"|"+it.Text)
	}
	return s
}

func eq(t *testing.T, label string, got, want shape) {
	t.Helper()
	if got.runStatus != want.runStatus || got.runLifecycle != want.runLifecycle || got.runFinalized != want.runFinalized {
		t.Errorf("%s: run shape differs: got %+v want %+v", label, got, want)
	}
	if got.sessStatus != want.sessStatus || got.sessLifecycle != want.sessLifecycle ||
		got.sessFinalized != want.sessFinalized || got.sessLinksRun != want.sessLinksRun {
		t.Errorf("%s: session shape differs: got %+v want %+v", label, got, want)
	}
	for _, pair := range []struct {
		name      string
		got, want []string
	}{
		{name: "authors", got: got.authors, want: want.authors},
		{name: "content_status", got: got.contentStatus, want: want.contentStatus},
		{name: "transcript", got: got.transcript, want: want.transcript},
	} {
		if len(pair.got) != len(pair.want) {
			t.Errorf("%s: %s length %d != %d", label, pair.name, len(pair.got), len(pair.want))
			continue
		}
		for i := range pair.got {
			if pair.got[i] != pair.want[i] {
				t.Errorf("%s: %s[%d] = %q, want %q", label, pair.name, i, pair.got[i], pair.want[i])
			}
		}
	}
	if len(got.roles) != len(want.roles) {
		t.Fatalf("%s: role count %d != %d", label, len(got.roles), len(want.roles))
	}
	for i := range got.roles {
		if got.roles[i] != want.roles[i] {
			t.Errorf("%s: role[%d] = %q, want %q", label, i, got.roles[i], want.roles[i])
		}
		if got.ordinals[i] != want.ordinals[i] {
			t.Errorf("%s: ordinal[%d] = %d, want %d", label, i, got.ordinals[i], want.ordinals[i])
		}
	}
}

// AC1 happy path: a complete City chain links and retrieves through the public
// operations, and the records it produces are indistinguishable in shape from a
// custom producer's — no field is City-required.
func TestCityVersusCustomProducerParity(t *testing.T) {
	ctx := context.Background()

	cityFake := NewFake("svc-city@tenant", "gc-city-01", "city")
	p := newCityProducer(t, cityFake, nil)
	res, err := p.Push(ctx, sampleChain())
	if err != nil {
		t.Fatalf("city Push: %v", err)
	}
	if res.Accepted != 3 || len(res.Sessions) != 1 || !res.Sessions[0].Finalized {
		t.Fatalf("city Push result = %+v, want 3 accepted and a finalized session", res)
	}
	cityShape := readShape(t, cityFake, res.RunTeamID, res.Sessions[0].TeamID)

	customFake := NewFake("svc-orch@tenant", "orchestrator-9", "custom")
	customRun, customSess := customUpload(t, customFake)
	customShape := readShape(t, customFake, customRun, customSess)

	eq(t, "city-vs-custom", cityShape, customShape)

	// The neutral IDs are server-minted on both sides and carry no source
	// spelling: a reader cannot tell which producer wrote the record.
	if res.RunTeamID == "city-run-7" || res.Sessions[0].TeamID == "city-sess-1" {
		t.Fatalf("neutral IDs echoed City native IDs: %q / %q", res.RunTeamID, res.Sessions[0].TeamID)
	}
	if customRun[:4] != res.RunTeamID[:4] {
		t.Fatalf("neutral run id prefixes differ by producer: %q vs %q", customRun, res.RunTeamID)
	}
}

// AC1 edge: City, rig and formula fields cannot become neutral authority.
func TestCityFactsCannotBecomeNeutralAuthority(t *testing.T) {
	chain := sampleChain()

	// Gate closed (the default): no City-shaped display string leaves at all.
	closed, err := (Mapper{Source: citySource()}).MapRun(chain.Run)
	if err != nil {
		t.Fatalf("MapRun: %v", err)
	}
	if closed.Title != "" {
		t.Fatalf("display gate closed but title = %q", closed.Title)
	}

	// Gate open: the City facts reach exactly one non-authoritative field.
	open, err := (Mapper{Source: citySource(), AllowDisplayContent: true}).MapRun(chain.Run)
	if err != nil {
		t.Fatalf("MapRun: %v", err)
	}
	if open.Title != "rig-alpha / formula-bravo" {
		t.Fatalf("title = %q", open.Title)
	}
	if open.SourceEntityID != "city-run-7" || open.ProjectID != "proj-a" || open.IssueID != "bd-1" {
		t.Fatalf("City facts displaced a source-native field: %+v", open)
	}

	// A City fact spelled like a neutral Team ID is refused rather than
	// carried: the adapter never lets City name a neutral identity.
	drift := chain.Run
	drift.Rig = "run_00000000000000ff"
	if _, err := (Mapper{Source: citySource(), AllowDisplayContent: true}).MapRun(drift); !errors.Is(err, ErrNeutralAuthority) {
		t.Fatalf("neutral-shaped rig: err = %v, want ErrNeutralAuthority", err)
	}

	// The session body carries no run reference: the link comes from the path
	// and the credential, so a City session cannot claim a foreign run.
	sessBody, err := (Mapper{Source: citySource()}).MapSession(chain.Sessions[0])
	if err != nil {
		t.Fatalf("MapSession: %v", err)
	}
	if err := ScanOutbound(sessBody, false); err != nil {
		t.Fatalf("ScanOutbound(session): %v", err)
	}
}

// A neutral record must be valid with no City in the picture: the custom
// producer's chain reads back complete on every operation this adapter uses.
func TestNeutralRecordsValidWithoutCity(t *testing.T) {
	f := NewFake("svc-orch@tenant", "orchestrator-9", "custom")
	runID, sessID := customUpload(t, f)
	s := readShape(t, f, runID, sessID)
	if !s.sessFinalized || !s.sessLinksRun || len(s.roles) != 3 || len(s.transcript) != 3 {
		t.Fatalf("custom-only chain incomplete: %+v", s)
	}
}
