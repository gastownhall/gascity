package cityartifact_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/pkg/cityartifact"
)

const (
	linkedRun     = "run_abcdefghij0123456789"
	linkedSession = "ses_abcdefghij0123456789"
	linkedProject = "proj-alpha"
	linkedIssue   = "bd-7"
)

func citySource() cityartifact.Source {
	return cityartifact.Source{SourceID: "src_city_1", Kind: "gascity", Epoch: 1}
}

func newCityProducer(t *testing.T, f *cityartifact.Fake) *cityartifact.Producer {
	t.Helper()
	p, err := cityartifact.NewProducer(f.Client("src_city_1", "gascity"),
		cityartifact.Mapper{Source: citySource()}, cityartifact.NewMemoryStore())
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	return p
}

func sampleArtifact() cityartifact.CityArtifact {
	return cityartifact.CityArtifact{
		ArtifactID: "city-artifact-1",
		Kind:       "transcript",
		MediaType:  "text/plain; charset=utf-8",
		Version:    1,
		ProjectID:  linkedProject,
		IssueID:    linkedIssue,
		RunID:      linkedRun,
		SessionID:  linkedSession,
		City:       "rustbelt",
		Rig:        "rig-3",
		Formula:    "review",
		Parts: []cityartifact.CityPart{
			{Sequence: 1, Bytes: []byte("first part\n")},
			{Sequence: 2, Bytes: []byte("second part\n")},
		},
		Complete: true,
	}
}

func authorizedFake() *cityartifact.Fake {
	f := cityartifact.NewFake()
	f.Authorize(linkedProject, linkedIssue, linkedRun, linkedSession)
	return f
}

// AC1 happy path: a valid linked artifact finalizes and its metadata reads back
// normalized.
func TestPushFinalizesAndReadsBack(t *testing.T) {
	ctx := context.Background()
	f := authorizedFake()
	p := newCityProducer(t, f)

	res, err := p.Push(ctx, sampleArtifact())
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !res.Finalized || res.Uploaded != 2 {
		t.Fatalf("want 2 uploaded and finalized, got %+v", res)
	}
	if !strings.HasPrefix(res.ArtifactID, "art_") {
		t.Fatalf("artifact id %q is not server-minted", res.ArtifactID)
	}
	if strings.Contains(res.ArtifactID, "city-artifact-1") {
		t.Fatalf("City minted the artifact id: %q", res.ArtifactID)
	}
	meta := res.Metadata
	if meta.Kind != "log" {
		t.Errorf("kind = %q, want the closed-vocabulary mapping of a City transcript", meta.Kind)
	}
	if meta.MediaType != "text/plain" {
		t.Errorf("media_type = %q, want the charset parameter dropped", meta.MediaType)
	}
	if meta.Status != "final" || !meta.Finalized() {
		t.Errorf("status = %q finalized = %v, want a sealed artifact", meta.Status, meta.Finalized())
	}
	if meta.SourceID != "src_city_1" {
		t.Errorf("source_id = %q, want the credential-derived source", meta.SourceID)
	}
	wantLinks := cityartifact.Links{ProjectID: linkedProject, IssueID: linkedIssue, RunID: linkedRun, SessionID: linkedSession}
	if meta.Links != wantLinks {
		t.Errorf("links = %+v, want %+v", meta.Links, wantLinks)
	}
	for _, fact := range []string{"rustbelt", "rig-3", "review"} {
		if strings.Contains(meta.Display, fact) {
			t.Errorf("City fact %q reached the public shape", fact)
		}
	}
}

// AC1: the same public contract, Team IDs, category scopes and provenance apply
// whether City or a custom producer uploaded. Two credentials, one tenant, one
// shape.
func TestCityAndCustomProducerAgree(t *testing.T) {
	ctx := context.Background()
	f := authorizedFake()

	city := newCityProducer(t, f)
	cityRes, err := city.Push(ctx, sampleArtifact())
	if err != nil {
		t.Fatalf("city Push: %v", err)
	}

	// The custom producer is not City: different source, no City vocabulary,
	// its own checkpoint. It uploads the same content via the same seam.
	customSource := cityartifact.Source{SourceID: "src_custom_1", Kind: "custom", Epoch: 1}
	custom, err := cityartifact.NewProducer(f.Client("src_custom_1", "custom-agent"),
		cityartifact.Mapper{Source: customSource}, cityartifact.NewMemoryStore())
	if err != nil {
		t.Fatalf("custom NewProducer: %v", err)
	}
	plain := sampleArtifact()
	plain.ArtifactID = "custom-artifact-1"
	plain.Kind = "log"
	plain.City, plain.Rig, plain.Formula = "", "", ""
	customRes, err := custom.Push(ctx, plain)
	if err != nil {
		t.Fatalf("custom Push: %v", err)
	}

	a, b := cityRes.Metadata, customRes.Metadata
	if a.ID == b.ID {
		t.Fatalf("both producers landed on artifact %q", a.ID)
	}
	if a.Kind != b.Kind || a.MediaType != b.MediaType || a.Links != b.Links {
		t.Errorf("shape diverged: city %+v custom %+v", a, b)
	}
	if a.Digest != b.Digest {
		t.Errorf("identical content hashed differently: %q vs %q", a.Digest, b.Digest)
	}
	if a.ByteSize != b.ByteSize || a.Status != b.Status {
		t.Errorf("state diverged: city %+v custom %+v", a, b)
	}
	if a.SourceID == b.SourceID {
		t.Errorf("provenance collapsed: both report source %q", a.SourceID)
	}
	if a.Producer != "gascity" || b.Producer != "custom-agent" {
		t.Errorf("producer attribution wrong: %q / %q", a.Producer, b.Producer)
	}

	// A custom producer reads a City artifact through the same public route:
	// City is one optional producer, not a private namespace.
	got, err := custom.Metadata(ctx, cityRes.ArtifactID)
	if err != nil {
		t.Fatalf("custom read of a City artifact: %v", err)
	}
	if got.ID != cityRes.ArtifactID {
		t.Errorf("read back %q, want %q", got.ID, cityRes.ArtifactID)
	}
}

// AC1 edge: a foreign link fails before a single byte is uploaded.
func TestForeignLinkFailsBeforeUpload(t *testing.T) {
	ctx := context.Background()
	f := cityartifact.NewFake()
	f.Authorize(linkedProject, linkedIssue, linkedRun) // session deliberately absent
	p := newCityProducer(t, f)

	res, err := p.Push(ctx, sampleArtifact())
	if !errors.Is(err, cityartifact.ErrNotReadable) {
		t.Fatalf("err = %v, want a foreign link refusal", err)
	}
	if res.Uploaded != 0 {
		t.Errorf("uploaded %d parts despite a foreign link", res.Uploaded)
	}
	for _, call := range f.Calls() {
		if call == cityartifact.OpUploadArtifactContent || call == cityartifact.OpFinalizeArtifact {
			t.Errorf("dispatched %s after a foreign link", call)
		}
	}
	if f.ArtifactCount() != 0 {
		t.Errorf("created %d artifacts despite a foreign link", f.ArtifactCount())
	}
}

// AC1 edge: a City-specific authority — a rig name where a server-minted link ID
// belongs — never reaches the wire.
func TestCityAuthorityRefusedBeforeAnyCall(t *testing.T) {
	ctx := context.Background()
	for name, mutate := range map[string]func(*cityartifact.CityArtifact){
		"city-native run id":  func(a *cityartifact.CityArtifact) { a.RunID = "rig-3/run-17" },
		"city locator":        func(a *cityartifact.CityArtifact) { a.ProjectID = "gc://rustbelt/proj" },
		"city name as link":   func(a *cityartifact.CityArtifact) { a.IssueID = "rig-3" },
		"city-native session": func(a *cityartifact.CityArtifact) { a.SessionID = "session-17" },
	} {
		t.Run(name, func(t *testing.T) {
			f := authorizedFake()
			p := newCityProducer(t, f)
			a := sampleArtifact()
			mutate(&a)
			if _, err := p.Push(ctx, a); !errors.Is(err, cityartifact.ErrCityAuthority) {
				t.Fatalf("err = %v, want ErrCityAuthority", err)
			}
			if len(f.Calls()) != 0 {
				t.Errorf("dispatched %v before refusing City authority", f.Calls())
			}
		})
	}
}

// AC1 edge: a raw upstream field cannot be smuggled into an outbound body.
func TestScanOutboundRefusesRawUpstreamFields(t *testing.T) {
	cases := map[string]struct {
		body any
		want error
	}{
		"server-minted id":  {map[string]any{"id": "art_1"}, cityartifact.ErrCityAuthority},
		"provenance":        {map[string]any{"source_id": "src_x"}, cityartifact.ErrCityAuthority},
		"City fact":         {map[string]any{"rig": "rig-3"}, cityartifact.ErrCityAuthority},
		"storage locator":   {map[string]any{"signed_url": "https://x/y"}, cityartifact.ErrCityAuthority},
		"content in a body": {map[string]any{"bytes": "aGk="}, cityartifact.ErrContentRoute},
		"nested content":    {map[string]any{"links": map[string]any{"body": "hi"}}, cityartifact.ErrContentRoute},
		"credential": {map[string]any{
			"kind": "file", "media_type": "text/plain",
			"links": map[string]any{"project_id": "p?signature=abc"},
		}, cityartifact.ErrCredentialLeak},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := cityartifact.ScanOutbound(tc.body); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	// The finalize body's asserted digest is the one permitted exception.
	if err := cityartifact.ScanOutbound(cityartifact.FinalizeRequest{Digest: "sha256:aa"}, "digest"); err != nil {
		t.Fatalf("finalize body refused: %v", err)
	}
	if err := cityartifact.ScanOutbound(cityartifact.FinalizeRequest{Digest: "sha256:aa"}); !errors.Is(err, cityartifact.ErrCityAuthority) {
		t.Fatalf("digest was permitted without being named")
	}
}

// The four categories are four reads. A metadata read returns no bytes, no
// evidence and no references, and content comes back only from the content call.
func TestCategorySeparation(t *testing.T) {
	ctx := context.Background()
	f := authorizedFake()
	p := newCityProducer(t, f)
	res, err := p.Push(ctx, sampleArtifact())
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	f.Seed(res.ArtifactID,
		[]cityartifact.EvidenceEntry{{ID: "ev1", Kind: "scan", Statement: "clean"}},
		[]cityartifact.Reference{{ID: "rf1", Kind: "issue", TargetID: linkedIssue}})

	evidence, err := p.Evidence(ctx, res.ArtifactID)
	if err != nil || len(evidence) != 1 {
		t.Fatalf("Evidence = %v, %v", evidence, err)
	}
	refs, err := p.References(ctx, res.ArtifactID)
	if err != nil || len(refs) != 1 {
		t.Fatalf("References = %v, %v", refs, err)
	}
	chunk, err := p.Content(ctx, res.ArtifactID, cityartifact.Range{})
	if err != nil {
		t.Fatalf("Content: %v", err)
	}
	if string(chunk.Bytes) != "first part\nsecond part\n" {
		t.Errorf("content = %q", chunk.Bytes)
	}
	meta, err := p.Metadata(ctx, res.ArtifactID)
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	if meta.ByteSize != int64(len(chunk.Bytes)) {
		t.Errorf("metadata byte_size %d disagrees with content length %d", meta.ByteSize, len(chunk.Bytes))
	}
	for op, wantCat := range map[string]cityartifact.Category{
		cityartifact.OpCreateArtifact:        cityartifact.CategoryMetadata,
		cityartifact.OpListArtifacts:         cityartifact.CategoryMetadata,
		cityartifact.OpGetArtifact:           cityartifact.CategoryMetadata,
		cityartifact.OpFinalizeArtifact:      cityartifact.CategoryMetadata,
		cityartifact.OpGetArtifactEvidence:   cityartifact.CategoryEvidence,
		cityartifact.OpGetArtifactReferences: cityartifact.CategoryReferences,
		cityartifact.OpUploadArtifactContent: cityartifact.CategoryContent,
		cityartifact.OpGetArtifactContent:    cityartifact.CategoryContent,
	} {
		got, ok := cityartifact.CategoryOf(op)
		if !ok || got != wantCat {
			t.Errorf("CategoryOf(%s) = %q, %v; want %q", op, got, ok, wantCat)
		}
	}
	if _, ok := cityartifact.CategoryOf("deleteArtifact"); ok {
		t.Error("an operation outside the frozen matrix was given a category")
	}
}

// An unfinalized artifact is not readable — not by its own producer, not by
// anyone.
func TestUnfinalizedArtifactIsNotReadable(t *testing.T) {
	ctx := context.Background()
	f := authorizedFake()
	p := newCityProducer(t, f)

	partial := sampleArtifact()
	partial.Complete = false
	res, err := p.Push(ctx, partial)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Finalized {
		t.Fatal("an incomplete manifest was finalized")
	}
	if res.Metadata.ID != "" {
		t.Fatalf("metadata returned for an unfinalized artifact: %+v", res.Metadata)
	}
	for _, read := range map[string]func() error{
		"metadata":   func() error { _, err := p.Metadata(ctx, res.ArtifactID); return err },
		"evidence":   func() error { _, err := p.Evidence(ctx, res.ArtifactID); return err },
		"references": func() error { _, err := p.References(ctx, res.ArtifactID); return err },
		"content":    func() error { _, err := p.Content(ctx, res.ArtifactID, cityartifact.Range{}); return err },
	} {
		if err := read(); !errors.Is(err, cityartifact.ErrNotReadable) {
			t.Errorf("read of an unfinalized artifact returned %v, want ErrNotReadable", err)
		}
	}
}

// An artifact is valid without City: no links, no City facts, plain kind.
func TestArtifactIsValidWithoutCity(t *testing.T) {
	ctx := context.Background()
	f := cityartifact.NewFake()
	p := newCityProducer(t, f)

	bare := cityartifact.CityArtifact{
		ArtifactID: "bare-1",
		Kind:       "unknown-city-kind",
		Version:    1,
		Parts:      []cityartifact.CityPart{{Sequence: 1, Bytes: []byte("x")}},
		Complete:   true,
	}
	res, err := p.Push(ctx, bare)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Metadata.Kind != "file" {
		t.Errorf("kind = %q, want the default rather than a raw City kind", res.Metadata.Kind)
	}
	if res.Metadata.MediaType != "application/octet-stream" {
		t.Errorf("media_type = %q, want the default", res.Metadata.MediaType)
	}
	if !res.Metadata.Links.Empty() {
		t.Errorf("links = %+v, want none", res.Metadata.Links)
	}
}
