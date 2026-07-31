package cityartifact_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/pkg/cityartifact"
)

// leakyUpstream is the kind of error a real transport hands back: a status line,
// a raw response body, a bearer token and a pre-signed URL, all in one string.
const leakyUpstream = `502 Bad Gateway: {"error":"presign failed","authorization":"Bearer sk-live-abc123def456",` +
	`"location":"https://bucket.s3.amazonaws.com/tenant/obj?X-Amz-Signature=deadbeefdeadbeef",` +
	`"stack":"art.storage.presign(/var/lib/art/blobs/ab/cd)"}`

func assertNoLeak(t *testing.T, err error) {
	t.Helper()
	got := err.Error()
	for _, secret := range []string{
		"sk-live-abc123def456", "X-Amz-Signature", "bucket.s3.amazonaws.com",
		"presign failed", "/var/lib/art/blobs", "502 Bad Gateway", "Bearer",
	} {
		if strings.Contains(got, secret) {
			t.Errorf("error leaked %q: %s", secret, got)
		}
	}
}

// AC3 happy path: a failure is observable as a safe, normalized refusal — the
// caller learns which operation failed and nothing else.
func TestUpstreamFailureIsNormalizedAndSafe(t *testing.T) {
	ctx := context.Background()
	for _, op := range []string{
		cityartifact.OpCreateArtifact,
		cityartifact.OpUploadArtifactContent,
		cityartifact.OpFinalizeArtifact,
	} {
		t.Run(op, func(t *testing.T) {
			f := authorizedFake()
			p := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), cityartifact.NewMemoryStore())
			f.FailNext(op, errors.New(leakyUpstream))
			_, err := p.Push(ctx, sampleArtifact())
			if !errors.Is(err, cityartifact.ErrUpstream) {
				t.Fatalf("err = %v, want ErrUpstream", err)
			}
			if !strings.Contains(err.Error(), op) {
				t.Errorf("error does not name the failed operation: %s", err)
			}
			assertNoLeak(t, err)
		})
	}
}

// AC3: the same normalization applies to every read, including the content read
// — the one call that legitimately handles bytes.
func TestReadFailuresAreNormalizedAndSafe(t *testing.T) {
	ctx := context.Background()
	f := authorizedFake()
	p := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), cityartifact.NewMemoryStore())
	res, err := p.Push(ctx, sampleArtifact())
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	reads := map[string]func() error{
		cityartifact.OpGetArtifact:           func() error { _, err := p.Metadata(ctx, res.ArtifactID); return err },
		cityartifact.OpGetArtifactEvidence:   func() error { _, err := p.Evidence(ctx, res.ArtifactID); return err },
		cityartifact.OpGetArtifactReferences: func() error { _, err := p.References(ctx, res.ArtifactID); return err },
		cityartifact.OpGetArtifactContent: func() error {
			_, err := p.Content(ctx, res.ArtifactID, cityartifact.Range{})
			return err
		},
		cityartifact.OpListArtifacts: func() error { _, _, err := p.List(ctx, cityartifact.ListQuery{}); return err },
	}
	for op, call := range reads {
		f.FailNext(op, fmt.Errorf("wrapped: %s", leakyUpstream))
		err := call()
		if !errors.Is(err, cityartifact.ErrUpstream) {
			t.Errorf("%s: err = %v, want ErrUpstream", op, err)
		}
		assertNoLeak(t, err)
	}
}

// AC3: a content denial is terminal for this artifact. No finalize follows, no
// other route is tried, and no second credential is reached for.
func TestContentDenialStopsWithoutFallback(t *testing.T) {
	ctx := context.Background()
	f := authorizedFake()
	store := cityartifact.NewMemoryStore()
	p := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), store)

	f.FailNext(cityartifact.OpUploadArtifactContent, cityartifact.ErrContentDenied)
	res, err := p.Push(ctx, sampleArtifact())
	if !errors.Is(err, cityartifact.ErrContentDenied) {
		t.Fatalf("err = %v, want ErrContentDenied", err)
	}
	if res.Uploaded != 0 || res.Finalized {
		t.Errorf("result = %+v, want nothing uploaded and nothing finalized", res)
	}
	calls := f.Calls()
	if len(calls) != 2 || calls[0] != cityartifact.OpCreateArtifact || calls[1] != cityartifact.OpUploadArtifactContent {
		t.Fatalf("calls = %v, want exactly the create and the denied upload", calls)
	}
	// The artifact stays unreadable: a denial cannot leave half an artifact
	// visible, because it was never finalized.
	if _, err := p.Metadata(ctx, res.ArtifactID); !errors.Is(err, cityartifact.ErrNotReadable) {
		t.Errorf("metadata of a denied artifact = %v, want ErrNotReadable", err)
	}
}

// AC3 edge: a scoped failure stays scoped. One artifact's denial leaves every
// other artifact — and every other producer's checkpoint — untouched, and the
// denied artifact resumes once the denial clears.
func TestScopedFailureDoesNotWidenOrSpread(t *testing.T) {
	ctx := context.Background()
	f := authorizedFake()
	denied := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), cityartifact.NewMemoryStore())
	neighborStore := cityartifact.NewMemoryStore()
	neighbor := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), neighborStore)

	f.FailNext(cityartifact.OpUploadArtifactContent, cityartifact.ErrContentDenied)
	bad := sampleArtifact()
	if _, err := denied.Push(ctx, bad); !errors.Is(err, cityartifact.ErrContentDenied) {
		t.Fatalf("err = %v, want ErrContentDenied", err)
	}

	// A different artifact on the same credential is unaffected.
	other := sampleArtifact()
	other.ArtifactID = "city-artifact-2"
	res, err := neighbor.Push(ctx, other)
	if err != nil {
		t.Fatalf("neighbor Push: %v", err)
	}
	if !res.Finalized {
		t.Error("a neighboring artifact was dragged down by an unrelated denial")
	}

	// The denied artifact resumes from its intact checkpoint, without a second
	// artifact and without a duplicate part.
	resumed, err := denied.Push(ctx, bad)
	if err != nil {
		t.Fatalf("resumed Push: %v", err)
	}
	if resumed.Uploaded != 2 || !resumed.Finalized {
		t.Errorf("resume = %+v, want both parts uploaded and finalized", resumed)
	}
	if f.ArtifactCount() != 2 {
		t.Errorf("tenant holds %d artifacts, want exactly the two pushed", f.ArtifactCount())
	}
	if got := f.PartCount(resumed.ArtifactID); got != 2 {
		t.Errorf("denied artifact holds %d parts after resume, want 2", got)
	}
}

// AC3: a timeout is an upstream failure with the context error preserved and the
// transport detail dropped.
func TestTimeoutIsNormalized(t *testing.T) {
	ctx := context.Background()
	f := authorizedFake()
	p := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), cityartifact.NewMemoryStore())
	f.FailNext(cityartifact.OpCreateArtifact,
		fmt.Errorf("post %s: %w", "https://api.internal/v1/artifacts?token=sk-live-1", context.DeadlineExceeded))

	_, err := p.Push(ctx, sampleArtifact())
	if !errors.Is(err, cityartifact.ErrUpstream) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want ErrUpstream wrapping the deadline", err)
	}
	if strings.Contains(err.Error(), "token=sk-live-1") || strings.Contains(err.Error(), "api.internal") {
		t.Errorf("timeout error leaked the request URL: %s", err)
	}
}

// malformedAPI answers successfully but with responses that do not hold
// together: no identity, or an acknowledgement against a different artifact.
type malformedAPI struct {
	cityartifact.API
	createID   string
	uploadID   string
	finalizeID string
	finalized  bool
}

func (m malformedAPI) CreateArtifact(context.Context, cityartifact.CreateRequest, string) (cityartifact.Artifact, error) {
	return cityartifact.Artifact{ID: m.createID}, nil
}

func (m malformedAPI) UploadArtifactContent(context.Context, string, cityartifact.Part, string) (cityartifact.Artifact, error) {
	return cityartifact.Artifact{ID: m.uploadID}, nil
}

func (m malformedAPI) FinalizeArtifact(context.Context, string, cityartifact.FinalizeRequest, string) (cityartifact.Artifact, error) {
	art := cityartifact.Artifact{ID: m.finalizeID}
	if m.finalized {
		at := time.Now()
		art.FinalizedAt = &at
	}
	return art, nil
}

// AC3: a malformed response is refused rather than recorded. An empty identity,
// a foreign acknowledgement and an unsealed finalize are all drift.
func TestMalformedResponsesAreRefused(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		api  malformedAPI
		want error
	}{
		"create without an id": {
			malformedAPI{createID: ""}, cityartifact.ErrIdentityDrift,
		},
		"part acknowledged against another artifact": {
			malformedAPI{createID: "art_1", uploadID: "art_2"}, cityartifact.ErrIdentityDrift,
		},
		"finalize acknowledged against another artifact": {
			malformedAPI{createID: "art_1", uploadID: "art_1", finalizeID: "art_9", finalized: true},
			cityartifact.ErrIdentityDrift,
		},
		"finalize returns an unsealed artifact": {
			malformedAPI{createID: "art_1", uploadID: "art_1", finalizeID: "art_1"},
			cityartifact.ErrUpstream,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := producerOn(t, tc.api, citySource(), cityartifact.NewMemoryStore())
			if _, err := p.Push(ctx, sampleArtifact()); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// AC3: a read-back that disagrees with what was sent is drift, not success.
func TestReadBackMismatchIsDrift(t *testing.T) {
	ctx := context.Background()
	f := authorizedFake()
	// A second producer's credential is enrolled under a different source, so the
	// artifact it reads back reports a source this producer did not send under.
	p := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), cityartifact.NewMemoryStore())
	res, err := p.Push(ctx, sampleArtifact())
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Metadata.SourceID != "src_city_1" {
		t.Fatalf("source_id = %q, want the enrolled source", res.Metadata.SourceID)
	}

	mismatched := cityartifact.Source{SourceID: "src_city_1", Kind: "gascity", Epoch: 1}
	other := producerOn(t, f.Client("src_other", "other"), mismatched, cityartifact.NewMemoryStore())
	if _, err := other.Push(ctx, sampleArtifact()); !errors.Is(err, cityartifact.ErrIdentityDrift) {
		t.Fatalf("err = %v, want ErrIdentityDrift when the read-back names another source", err)
	}
}

// AC3: an idempotency key derived from anything volatile would show up here as a
// conflict. It never does, which is what makes the derivation the dedup contract
// rather than a hope.
func TestNoIdempotencyConflictAcrossRetries(t *testing.T) {
	ctx := context.Background()
	f := authorizedFake()
	store := cityartifact.NewMemoryStore()
	p := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), store)

	for i := 0; i < 4; i++ {
		if _, err := p.Push(ctx, sampleArtifact()); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}
	fresh := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), cityartifact.NewMemoryStore())
	if _, err := fresh.Push(ctx, sampleArtifact()); err != nil {
		if errors.Is(err, cityartifact.ErrIdempotencyConflict) {
			t.Fatalf("derived key conflicted on identical input: %v", err)
		}
		t.Fatalf("fresh Push: %v", err)
	}
}
