package cityneutral

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// AC3 happy path: uploader, source, represented author and tenant ownership
// stay four distinct things on an accepted record.
func TestUploaderSourceAndAuthorStayDistinct(t *testing.T) {
	f := NewFake("svc-city@acme", "gc-city-01", "city")
	p := newCityProducer(t, f, nil)
	res, err := p.Push(context.Background(), sampleChain())
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	recs, err := f.ListTranscriptRecords(context.Background(), res.Sessions[0].TeamID)
	if err != nil {
		t.Fatalf("ListTranscriptRecords: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("got %d records, want 3", len(recs))
	}
	first := recs[0]
	if first.SourceID != "gc-city-01" || first.SourceKind != "city" {
		t.Fatalf("source not credential-derived: %+v", first)
	}
	if first.Author == nil || first.Author.Ref != "u-42" || first.Author.Kind != "human" {
		t.Fatalf("represented author not preserved: %+v", first.Author)
	}
	if first.Author.Ref == f.Uploader || first.SourceID == f.Uploader {
		t.Fatalf("uploader collapsed into author or source: uploader=%q", f.Uploader)
	}
	// The record with no City author keeps a nil author rather than being
	// silently attributed to the uploader or the source.
	if recs[2].Author != nil {
		t.Fatalf("unauthored record acquired an author: %+v", recs[2].Author)
	}
}

// AC3 edge: an actor or key ID cannot become the uploader, the source, or an
// authorization input. The adapter's own claim about who spoke is attribution
// and is carried in a body field the server treats as non-authoritative; the
// source on the stored record comes from the credential regardless.
func TestActorCannotBecomeUploaderOrSource(t *testing.T) {
	f := NewFake("svc-city@acme", "gc-city-01", "city")
	p := newCityProducer(t, f, nil)

	chain := sampleChain()
	// A City author impersonating the uploader and the enrolled source.
	chain.Sessions[0].Records[0].Author = &ContributorRef{Kind: "human", Ref: "svc-city@acme"}
	chain.Sessions[0].Records[1].Author = &ContributorRef{Kind: "agent", Ref: "gc-city-01"}

	res, err := p.Push(context.Background(), chain)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	recs, err := f.ListTranscriptRecords(context.Background(), res.Sessions[0].TeamID)
	if err != nil {
		t.Fatalf("ListTranscriptRecords: %v", err)
	}
	for i, r := range recs[:2] {
		if r.SourceID != "gc-city-01" || r.SourceKind != "city" {
			t.Fatalf("record %d source came from the body: %+v", i, r)
		}
	}
	// Attribution that happens to spell the uploader is still only attribution:
	// it changed no ownership field on the stored record.
	if recs[0].SessionID != res.Sessions[0].TeamID {
		t.Fatalf("record ownership drifted: %+v", recs[0])
	}
}

// AC3: the outbound scanner refuses credential evidence, ownership assertions
// and raw content outside the transcript-record route.
func TestScanOutboundRefusals(t *testing.T) {
	cases := []struct {
		name         string
		body         map[string]any
		allowContent bool
		want         error
	}{
		{
			name: "bearer token in a field",
			body: map[string]any{"source_entity_id": "bearer AAAAAAAAAAAAAAAAAAAA"},
			want: ErrCredentialLeak,
		},
		{
			name: "aws secret arn nested under an author",
			body: map[string]any{"author": map[string]any{"ref": "arn:aws:secretsmanager:us-east-1:1:secret:x"}},
			want: ErrCredentialLeak,
		},
		{
			name: "uploader asserted by the producer",
			body: map[string]any{"uploaded_by": "svc-city@acme"},
			want: ErrNeutralAuthority,
		},
		{
			name: "signing key id smuggled into a body",
			body: map[string]any{"key_id": "city-2026-07"},
			want: ErrNeutralAuthority,
		},
		{
			name: "tenant asserted by the producer",
			body: map[string]any{"tenant_id": "acme"},
			want: ErrNeutralAuthority,
		},
		{
			name: "neutral id asserted by the producer",
			body: map[string]any{"id": "run_00000000000000ff"},
			want: ErrNeutralAuthority,
		},
		{
			name: "raw content on a non-transcript route",
			body: map[string]any{"text": "secret plan"},
			want: ErrContentRoute,
		},
		{
			name:         "raw content on the transcript route is allowed",
			body:         map[string]any{"text": "secret plan"},
			allowContent: true,
			want:         nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ScanOutbound(tc.body, tc.allowContent)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// AC3: nothing this adapter serializes carries a credential, a key id, or an
// ownership assertion — checked on the bytes, not on the struct.
func TestSerializedBodiesCarryNoCredentialOrOwnership(t *testing.T) {
	m := Mapper{Source: citySource(), AllowDisplayContent: true, AllowRawContent: true}
	chain := sampleChain()

	runBody, err := m.MapRun(chain.Run)
	if err != nil {
		t.Fatalf("MapRun: %v", err)
	}
	sessBody, err := m.MapSession(chain.Sessions[0])
	if err != nil {
		t.Fatalf("MapSession: %v", err)
	}
	recBody, err := m.MapRecord(chain.Sessions[0].Records[0])
	if err != nil {
		t.Fatalf("MapRecord: %v", err)
	}

	forbidden := []string{
		"uploaded_by", "uploader", "tenant", "workspace", "key_id",
		"actor", "principal", "authorization", "credential", `"source_id"`, `"id"`,
	}
	for _, body := range []any{runBody, sessBody, recBody} {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		for _, f := range forbidden {
			if strings.Contains(string(raw), f) {
				t.Fatalf("body %s carries %q", raw, f)
			}
		}
	}

	// The run and session bodies carry no raw content at all; only the
	// transcript body may, and only with the content gate open.
	if err := ScanOutbound(runBody, false); err != nil {
		t.Fatalf("ScanOutbound(run): %v", err)
	}
	if err := ScanOutbound(sessBody, false); err != nil {
		t.Fatalf("ScanOutbound(session): %v", err)
	}
	if err := ScanOutbound(recBody, true); err != nil {
		t.Fatalf("ScanOutbound(record): %v", err)
	}
	if err := ScanOutbound(recBody, false); !errors.Is(err, ErrContentRoute) {
		t.Fatalf("record body on a non-content route: err = %v, want ErrContentRoute", err)
	}
}

// AC3: with the content gate closed the text never leaves, and the record still
// goes — a transcript with a known-withheld turn beats one with a hole in it.
func TestContentGateWithholdsTextWithoutDroppingTheRecord(t *testing.T) {
	f := NewFake("svc-city@acme", "gc-city-01", "city")
	p := newCityProducer(t, f, func(m *Mapper) { m.AllowRawContent = false })
	res, err := p.Push(context.Background(), sampleChain())
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	items, err := f.GetSessionTranscript(context.Background(), res.Sessions[0].TeamID)
	if err != nil {
		t.Fatalf("GetSessionTranscript: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("got %d transcript items, want 3", len(items))
	}
	for i, it := range items {
		if it.Text != "" {
			t.Fatalf("item %d leaked text %q with the content gate closed", i, it.Text)
		}
		if it.ContentStatus != "missing" {
			t.Fatalf("item %d content status = %q, want missing", i, it.ContentStatus)
		}
	}
}

// A City field carrying credential evidence is refused at the producer with the
// source field named, instead of becoming a 4xx an operator has to correlate.
func TestCredentialEvidenceInCityFieldsIsRefused(t *testing.T) {
	m := Mapper{Source: citySource(), AllowRawContent: true}
	chain := sampleChain()
	chain.Run.RunID = "vault://prod/runs/7"
	if _, err := m.MapRun(chain.Run); !errors.Is(err, ErrCredentialLeak) {
		t.Fatalf("err = %v, want ErrCredentialLeak", err)
	}

	rec := sampleChain().Sessions[0].Records[0]
	rec.Text = "paste this: sk-AAAAAAAAAAAAAAAAAAAAAAAA"
	if _, err := m.MapRecord(rec); !errors.Is(err, ErrCredentialLeak) {
		t.Fatalf("text: err = %v, want ErrCredentialLeak", err)
	}
}
