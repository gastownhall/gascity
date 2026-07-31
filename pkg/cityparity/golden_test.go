package cityparity

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/pkg/cityartifact"
	"github.com/gastownhall/gascity/pkg/cityinference"
	"github.com/gastownhall/gascity/pkg/cityneutral"
)

var (
	t0   = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	tEnd = t0.Add(9 * time.Minute)
)

const (
	tenant           = "org_alpha"
	linkedProject    = "proj-a"
	linkedIssue      = "bd-1"
	mediaType        = "text/plain; charset=utf-8"
	neutralMediaType = "text/plain"
)

var partBytes = [][]byte{[]byte("first part\n"), []byte("second part\n")}

// producerStyle is one way of producing the same work. City is one of them and
// deliberately not the privileged one: the custom style speaks the same API
// with its own enrolled identity, its own native IDs and no City code in the
// path at all.
type producerStyle struct {
	name       string
	sourceID   string
	sourceKind string
	uploader   string
	// native IDs this style uses on its own side of the boundary.
	runID, sessionID string
	msgIDs           []string
	invSession       string
	invReq           string
}

var (
	cityStyle = producerStyle{
		name: "city", sourceID: "src_city_1", sourceKind: "city", uploader: "svc-city",
		runID: "city-run-7", sessionID: "city-sess-1",
		msgIDs: []string{"m1", "m2", "m3"}, invSession: "sess-1", invReq: "req-1",
	}
	customStyle = producerStyle{
		name: "custom", sourceID: "src_custom_1", sourceKind: "orchestrator", uploader: "svc-orch",
		runID: "orch-run-7", sessionID: "orch-sess-1",
		msgIDs: []string{"e1", "e2", "e3"}, invSession: "sess-1", invReq: "req-1",
	}
)

// replacer rewrites everything that is legitimately style-specific — the
// enrolled source identity and the source-native IDs — into placeholders, so
// what remains in a rendered line is only what the contract says both styles
// must agree on.
func (s producerStyle) replacer() *strings.Replacer {
	// Most specific first: a source kind of "city" is a prefix of the native
	// run ID "city-run-7", and replacing the kind first would hide the very
	// difference this placeholder exists to normalize.
	pairs := []string{
		s.runID, "<native-run>",
		s.sessionID, "<native-session>",
	}
	for i, m := range s.msgIDs {
		pairs = append(pairs, m, fmt.Sprintf("<native-msg-%d>", i+1))
	}
	pairs = append(pairs,
		s.sourceID, "<source-id>",
		s.sourceKind, "<source-kind>",
		s.uploader, "<uploader>",
	)
	return strings.NewReplacer(pairs...)
}

// symbols renders server-minted Team IDs as first-seen ordinals. Parity is
// about a Team ID being opaque, server-minted and consistently linked — never
// about two producers being handed the same string.
type symbols struct {
	seen map[string]string
	n    int
}

func newSymbols() *symbols { return &symbols{seen: map[string]string{}} }

func (s *symbols) of(id string) string {
	if id == "" {
		return "-"
	}
	if sym, ok := s.seen[id]; ok {
		return sym
	}
	s.n++
	s.seen[id] = fmt.Sprintf("#%d", s.n)
	return s.seen[id]
}

// chain is the City-native input for the neutral leg.
func (s producerStyle) chain() cityneutral.CityChain {
	return cityneutral.CityChain{
		Run: cityneutral.CityRun{
			RunID: s.runID, Status: "succeeded", Started: t0, Ended: &tEnd,
			ProjectID: linkedProject, IssueID: linkedIssue, Version: 3,
			Rig: "rig-alpha", Formula: "formula-bravo", City: "rustbelt",
		},
		Sessions: []cityneutral.CitySession{{
			SessionID: s.sessionID, Status: "succeeded", Started: t0, Ended: &tEnd,
			Version: 2, Complete: true,
			Records: []cityneutral.CityRecord{
				{MessageID: s.msgIDs[0], Ordinal: 1, Role: "user", At: t0.Add(time.Minute), Text: "kick it off"},
				{MessageID: s.msgIDs[1], Ordinal: 2, Role: "agent", At: t0.Add(2 * time.Minute), Text: "on it"},
				{MessageID: s.msgIDs[2], Ordinal: 3, Role: "tool", At: t0.Add(3 * time.Minute), ContentRef: "blob-3"},
			},
		}},
	}
}

func u64(v uint64) *uint64 { return &v }

// cityNeutralLeg runs the City adapter. customNeutralLeg does the same work
// with no City code in the path: it builds the neutral bodies itself and mints
// its own idempotency keys, which is what makes the comparison a comparison of
// two implementations rather than of one implementation wearing two hats.
func cityNeutralLeg(t *testing.T, f *cityneutral.Fake, s producerStyle) (string, string) {
	t.Helper()
	p, err := cityneutral.NewProducer(f, cityneutral.Mapper{
		Source:          cityneutral.Source{SourceID: s.sourceID, Kind: s.sourceKind, Epoch: 1},
		AllowRawContent: true,
	}, cityneutral.NewMemoryStore())
	if err != nil {
		t.Fatalf("neutral NewProducer: %v", err)
	}
	res, err := p.Push(context.Background(), s.chain())
	if err != nil {
		t.Fatalf("neutral Push: %v", err)
	}
	if len(res.Sessions) != 1 {
		t.Fatalf("neutral Push produced %d sessions", len(res.Sessions))
	}
	return res.RunTeamID, res.Sessions[0].TeamID
}

func customNeutralLeg(t *testing.T, api cityneutral.API, s producerStyle) (string, string) {
	t.Helper()
	ctx := context.Background()
	run, err := api.UpsertRun(ctx, cityneutral.Upsert{
		SourceEntityID: s.runID, SourceVersion: 3, Epoch: 1,
		Status: cityneutral.StatusSucceeded, Lifecycle: cityneutral.LifecyclePartial,
		StartedAt: t0, EndedAt: &tEnd, ProjectID: linkedProject, IssueID: linkedIssue,
		Coverage: cityneutral.CoverageKnown,
	}, "11111111-1111-5111-8111-111111111111")
	if err != nil {
		t.Fatalf("custom UpsertRun: %v", err)
	}
	sess, err := api.UpsertRunSession(ctx, run.ID, cityneutral.Upsert{
		SourceEntityID: s.sessionID, SourceVersion: 2, Epoch: 1,
		Status: cityneutral.StatusSucceeded, Lifecycle: cityneutral.LifecyclePartial,
		StartedAt: t0, EndedAt: &tEnd, Coverage: cityneutral.CoverageKnown,
	}, "22222222-2222-5222-8222-222222222222")
	if err != nil {
		t.Fatalf("custom UpsertRunSession: %v", err)
	}
	records := []cityneutral.TranscriptRecordIngest{
		{
			SourceMessageID: s.msgIDs[0], SourceVersion: 1, Epoch: 1, Ordinal: u64(1),
			Role: cityneutral.RoleUser, OccurredAt: t0.Add(time.Minute), Text: "kick it off",
		},
		{
			SourceMessageID: s.msgIDs[1], SourceVersion: 1, Epoch: 1, Ordinal: u64(2),
			Role: cityneutral.RoleAssistant, OccurredAt: t0.Add(2 * time.Minute), Text: "on it",
		},
		{
			SourceMessageID: s.msgIDs[2], SourceVersion: 1, Epoch: 1, Ordinal: u64(3),
			Role: cityneutral.RoleTool, OccurredAt: t0.Add(3 * time.Minute), ContentRef: "blob-3",
		},
	}
	for i, rec := range records {
		if _, err := api.CreateTranscriptRecord(ctx, sess.ID, rec,
			fmt.Sprintf("3333333%d-3333-5333-8333-333333333333", i)); err != nil {
			t.Fatalf("custom CreateTranscriptRecord %d: %v", i, err)
		}
	}
	if _, err := api.FinalizeSession(ctx, sess.ID, "44444444-4444-5444-8444-444444444444"); err != nil {
		t.Fatalf("custom FinalizeSession: %v", err)
	}
	return run.ID, sess.ID
}

func renderNeutral(t *testing.T, api cityneutral.API, sym *symbols, runID, sessID string) []string {
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
	items, err := api.GetSessionTranscript(ctx, sessID)
	if err != nil {
		t.Fatalf("GetSessionTranscript: %v", err)
	}
	out := []string{
		fmt.Sprintf("run id=%s source=%s/%s native=%s status=%s lifecycle=%s epoch=%d final=%t etag_present=%t",
			sym.of(run.ID), run.SourceKind, run.SourceID, run.SourceRunID, run.Status, run.Lifecycle,
			run.Epoch, run.Finalized, run.ETag != ""),
		fmt.Sprintf("session id=%s run=%s source=%s/%s native=%s status=%s lifecycle=%s epoch=%d final=%t",
			sym.of(sess.ID), sym.of(sess.RunID), sess.SourceKind, sess.SourceID, sess.SourceSessionID,
			sess.Status, sess.Lifecycle, sess.Epoch, sess.Finalized),
	}
	for _, it := range items {
		ordinal := "-"
		if it.Ordinal != nil {
			ordinal = fmt.Sprintf("%d", *it.Ordinal)
		}
		out = append(out, fmt.Sprintf("transcript id=%s native=%s role=%s ordinal=%s content=%s text_present=%t",
			sym.of(it.RecordID), it.SourceMessageID, it.Role, ordinal, it.ContentStatus, it.Text != ""))
	}
	return out
}

func (s producerStyle) artifact(runID, sessID string) cityartifact.CityArtifact {
	return cityartifact.CityArtifact{
		ArtifactID: s.runID + "-artifact", Kind: "transcript", MediaType: mediaType, Version: 1,
		ProjectID: linkedProject, IssueID: linkedIssue, RunID: runID, SessionID: sessID,
		City: "rustbelt", Rig: "rig-3", Formula: "review",
		Parts: []cityartifact.CityPart{
			{Sequence: 1, Bytes: partBytes[0]},
			{Sequence: 2, Bytes: partBytes[1]},
		},
		Complete: true,
	}
}

func cityArtifactLeg(t *testing.T, f *cityartifact.Fake, s producerStyle, runID, sessID string) string {
	t.Helper()
	p, err := cityartifact.NewProducer(f.Client(s.sourceID, s.sourceKind),
		cityartifact.Mapper{Source: cityartifact.Source{SourceID: s.sourceID, Kind: s.sourceKind, Epoch: 1}},
		cityartifact.NewMemoryStore())
	if err != nil {
		t.Fatalf("artifact NewProducer: %v", err)
	}
	res, err := p.Push(context.Background(), s.artifact(runID, sessID))
	if err != nil {
		t.Fatalf("artifact Push: %v", err)
	}
	return res.ArtifactID
}

func customArtifactLeg(t *testing.T, api cityartifact.API, runID, sessID string) string {
	t.Helper()
	ctx := context.Background()
	// The custom producer speaks the neutral vocabulary directly; the City
	// adapter reaches the same values by mapping its own ("transcript" is
	// City's word for an artifact of neutral kind "log"). Parity is asserted at
	// the neutral layer, which is the only layer both producers share.
	art, err := api.CreateArtifact(ctx, cityartifact.CreateRequest{
		Kind: "log", MediaType: neutralMediaType,
		Links: cityartifact.Links{ProjectID: linkedProject, IssueID: linkedIssue, RunID: runID, SessionID: sessID},
	}, "55555555-5555-5555-8555-555555555555")
	if err != nil {
		t.Fatalf("custom CreateArtifact: %v", err)
	}
	for i, b := range partBytes {
		part := cityartifact.Part{Bytes: b, MediaType: neutralMediaType, Sequence: i + 1}
		if _, err := api.UploadArtifactContent(ctx, art.ID, part,
			fmt.Sprintf("6666666%d-6666-5666-8666-666666666666", i)); err != nil {
			t.Fatalf("custom UploadArtifactContent %d: %v", i, err)
		}
	}
	if _, err := api.FinalizeArtifact(ctx, art.ID, cityartifact.FinalizeRequest{},
		"77777777-7777-5777-8777-777777777777"); err != nil {
		t.Fatalf("custom FinalizeArtifact: %v", err)
	}
	return art.ID
}

func renderArtifact(t *testing.T, api cityartifact.API, sym *symbols, artifactID string) []string {
	t.Helper()
	ctx := context.Background()
	meta, err := api.GetArtifact(ctx, artifactID)
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	listed, _, err := api.ListArtifacts(ctx, cityartifact.ListQuery{})
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	return []string{
		fmt.Sprintf("artifact id=%s kind=%s media=%s status=%s bytes=%d digest_present=%t source=%s producer=%s",
			sym.of(meta.ID), meta.Kind, meta.MediaType, meta.Status, meta.ByteSize, meta.Digest != "",
			meta.SourceID, meta.Producer),
		fmt.Sprintf("artifact-links project=%s issue=%s run=%s session=%s",
			meta.Links.ProjectID, meta.Links.IssueID, sym.of(meta.Links.RunID), sym.of(meta.Links.SessionID)),
		fmt.Sprintf("artifact-list count=%d", len(listed)),
	}
}

func (s producerStyle) invocation(runID, sessID, recID string) cityinference.CityInvocation {
	return cityinference.CityInvocation{
		SessionNativeID: s.invSession, UpstreamReqID: s.invReq,
		Provider: "anthropic", Model: "m-1", Status: "succeeded",
		Started: t0, Ended: &tEnd,
		RunTeamID: runID, SessionTeamID: sessID, TranscriptRecordID: recID,
		Usage: &cityinference.UsageObservation{
			InputTokens: u64(120), OutputTokens: u64(30), CostMicros: u64(4500),
		},
	}
}

func newInferenceAPI(runID, sessID, recID string) *cityinference.FakeAPI {
	ws := cityinference.NewFakeWorkspace()
	ws.AddChain(runID, sessID, recID)
	return cityinference.NewFakeAPI(tenant, map[string]*cityinference.FakeWorkspace{tenant: ws})
}

func inferenceSource(s producerStyle) cityinference.Source {
	return cityinference.Source{SourceID: s.sourceID, Kind: s.sourceKind, Tenant: tenant, Epoch: 1}
}

func cityInferenceLeg(t *testing.T, api cityinference.API, s producerStyle, runID, sessID, recID string) string {
	t.Helper()
	p, err := cityinference.NewProducer(api, cityinference.Mapper{Source: inferenceSource(s)},
		cityinference.NewMemoryStore())
	if err != nil {
		t.Fatalf("inference NewProducer: %v", err)
	}
	res, err := p.Push(context.Background(), []cityinference.CityInvocation{s.invocation(runID, sessID, recID)})
	if err != nil {
		t.Fatalf("inference Push: %v", err)
	}
	if len(res.Uploaded) != 1 {
		t.Fatalf("inference Push uploaded %d records", len(res.Uploaded))
	}
	return res.Uploaded[0].InferenceTeamID
}

// customInferenceLeg hand-builds the request: no Mapper, no Producer, no City
// type in the path except the frozen wire structs the server itself defines.
func customInferenceLeg(t *testing.T, api cityinference.API, s producerStyle, runID, sessID, recID string) string {
	t.Helper()
	id := cityinference.NativeIdentity{
		Kind: cityinference.NativeIdentityKind, Tenant: tenant,
		SessionID: s.invSession, UpstreamReqID: s.invReq,
	}
	ext, err := cityinference.DeriveExternalInferenceID(id)
	if err != nil {
		t.Fatalf("custom DeriveExternalInferenceID: %v", err)
	}
	req := cityinference.CreateInferenceRequest{
		NativeIdentity: id, ExternalInferenceID: ext,
		Provider: "anthropic", Model: "m-1", Outcome: cityinference.OutcomeOK,
		StartedAt: t0, EndedAt: &tEnd, Epoch: 1,
		SessionTeamID: sessID, RunTeamID: runID, TranscriptRecordID: recID,
		Usage: &cityinference.UsageObservation{
			InputTokens: u64(120), OutputTokens: u64(30), CostMicros: u64(4500),
		},
	}
	if err := cityinference.Preflight(req); err != nil {
		t.Fatalf("custom Preflight refused a legal request: %v", err)
	}
	got, err := api.CreateInference(context.Background(), req, "custom-inference-key-1")
	if err != nil {
		t.Fatalf("custom CreateInference: %v", err)
	}
	return got.ID
}

func renderInference(t *testing.T, api cityinference.API, sym *symbols, inferenceID string) []string {
	t.Helper()
	ctx := context.Background()
	inf, err := api.GetInference(ctx, inferenceID)
	if err != nil {
		t.Fatalf("GetInference: %v", err)
	}
	page, err := api.ListInferences(ctx, cityinference.ListFilter{})
	if err != nil {
		t.Fatalf("ListInferences: %v", err)
	}
	coverage := make([]string, 0, len(cityinference.CoverageFieldGroups()))
	for _, g := range cityinference.CoverageFieldGroups() {
		coverage = append(coverage, g+"="+inf.Coverage[g])
	}
	tokens := "-"
	if inf.TokenUsage != nil {
		tokens = "present"
	}
	// external is rendered RAW on purpose: the derived handle is a function of
	// the native triplet alone, so both styles must produce the same string.
	// If producing through City changed it, City would be an identity authority.
	return []string{
		fmt.Sprintf("inference id=%s external=%s run=%s session=%s record=%s outcome=%s scope=%s fold=%t",
			sym.of(inf.ID), inf.ExternalInferenceID, sym.of(inf.RunID), sym.of(inf.SessionID),
			sym.of(inf.TranscriptRecordID), inf.Outcome, inf.IdentityScope, inf.FoldEligible),
		fmt.Sprintf("inference-usage tokens=%s completeness=%s epoch=%d contributions=%d",
			tokens, inf.Completeness, inf.Epoch, len(inf.Contributions)),
		"inference-coverage " + strings.Join(coverage, " "),
		fmt.Sprintf("inference-list count=%d", len(page.Items)),
	}
}

// graph produces the whole normalized cross-adapter chain for one style. The
// three adapters are wired to each other through server-minted Team IDs only,
// which is the same wiring a real deployment has and the reason a City fact
// cannot become load bearing between them.
func graph(t *testing.T, s producerStyle, city bool) []string {
	t.Helper()
	sym := newSymbols()

	nf := cityneutral.NewFake(s.uploader, s.sourceID, s.sourceKind)
	var runID, sessID string
	if city {
		runID, sessID = cityNeutralLeg(t, nf, s)
	} else {
		runID, sessID = customNeutralLeg(t, nf, s)
	}
	lines := renderNeutral(t, nf, sym, runID, sessID)

	records, err := nf.ListTranscriptRecords(context.Background(), sessID)
	if err != nil {
		t.Fatalf("ListTranscriptRecords: %v", err)
	}
	if len(records) == 0 {
		t.Fatal("no transcript records to link an inference to")
	}
	recID := records[0].ID

	af := cityartifact.NewFake()
	af.Authorize(linkedProject, linkedIssue, runID, sessID)
	aapi := af.Client(s.sourceID, s.sourceKind)
	var artifactID string
	if city {
		artifactID = cityArtifactLeg(t, af, s, runID, sessID)
	} else {
		artifactID = customArtifactLeg(t, aapi, runID, sessID)
	}
	lines = append(lines, renderArtifact(t, aapi, sym, artifactID)...)

	iapi := newInferenceAPI(runID, sessID, recID)
	var inferenceID string
	if city {
		inferenceID = cityInferenceLeg(t, iapi, s, runID, sessID, recID)
	} else {
		inferenceID = customInferenceLeg(t, iapi, s, runID, sessID, recID)
	}
	lines = append(lines, renderInference(t, iapi, sym, inferenceID)...)

	rep := s.replacer()
	for i, l := range lines {
		lines[i] = rep.Replace(l)
	}
	return lines
}

// AC1 happy path: the two producer styles yield equivalent normalized
// resources across all three adapters at once.
func TestCrossProducerGoldenGraph(t *testing.T) {
	cityLines := graph(t, cityStyle, true)
	customLines := graph(t, customStyle, false)

	if len(cityLines) != len(customLines) {
		t.Fatalf("graph shape differs: city has %d lines, custom has %d\ncity:\n%s\ncustom:\n%s",
			len(cityLines), len(customLines), strings.Join(cityLines, "\n"), strings.Join(customLines, "\n"))
	}
	for i := range cityLines {
		if cityLines[i] != customLines[i] {
			t.Errorf("line %d diverges:\n city:   %s\n custom: %s", i, cityLines[i], customLines[i])
		}
	}
}

// AC1: no City-shaped fact survives into any normalized resource. The City
// chain carries a city, a rig and a formula on every leg; if any of them
// reached a neutral record, a custom producer would have to satisfy a City
// schema to reach parity.
func TestCityFactsNeverReachNeutralResources(t *testing.T) {
	lines := strings.Join(graph(t, cityStyle, true), "\n")
	for _, fact := range []string{"rustbelt", "rig-alpha", "formula-bravo", "rig-3", "review"} {
		if strings.Contains(lines, fact) {
			t.Errorf("City-only fact %q reached a neutral resource:\n%s", fact, lines)
		}
	}
}

// AC1 edge: every adapter has an outbound guard, and all three refuse rather
// than truncate. A single adapter that let free-form egress through would make
// the parity claim style-dependent.
func TestAllThreeAdaptersGuardOutboundEgress(t *testing.T) {
	if err := cityneutral.ScanOutbound(cityneutral.TranscriptRecordIngest{
		SourceMessageID: "m1", Role: cityneutral.RoleUser, OccurredAt: t0, Text: "secret transcript text",
	}, false); err == nil {
		t.Error("cityneutral.ScanOutbound admitted free-form content with content egress off")
	}
	if err := cityartifact.ScanOutbound(cityartifact.CreateRequest{
		Kind: "transcript", MediaType: mediaType,
		Links: cityartifact.Links{RunID: "https://bucket.s3.amazonaws.com/o?X-Amz-Signature=dead"},
	}); err == nil {
		t.Error("cityartifact.ScanOutbound admitted a pre-signed locator")
	}
	if err := cityinference.ScanForbidden([]byte(`{"prompt":"do the thing","completion":"done"}`)); err == nil {
		t.Error("cityinference.ScanForbidden admitted prompt and completion text")
	}
}
