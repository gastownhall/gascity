package cityneutral

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Fake is an in-memory stand-in for the neutral v1 API.
//
// It is not a mock of this package's expectations: it models the parts of the
// server contract a producer can actually get wrong — server-minted Team IDs,
// idempotent replay keyed on Idempotency-Key, conflict on a changed payload
// under a used key, one-way finalize, and ownership derived from the credential
// rather than from the body. A producer that satisfies this Fake is a producer
// whose failures against the real server are transport failures.
//
// It lives outside _test.go on purpose: a custom orchestrator replaying the
// same conformance chain needs it too, and the parity claim is only meaningful
// if both producers run against the SAME server model.
type Fake struct {
	mu sync.Mutex

	// Uploader is who the credential says is calling. The server derives it;
	// no request body may set it, and this Fake never reads it from one.
	Uploader string
	// SourceID and SourceKind are the enrolled producer identity bound to the
	// credential. Also server-derived, also never read from a body.
	SourceID   string
	SourceKind string

	// Fail, when set, is returned by the next call and then cleared. It models
	// a transient fault — a credential mid-rotation, a 5xx — so a test can prove
	// the producer neither advances nor duplicates across it.
	Fail error

	seq      uint64
	runs     map[string]*Run
	runIdx   map[string]string
	sessions map[string]*Session
	sessIdx  map[string]string
	records  map[string][]TranscriptRecord
	text     map[string]string
	keys     map[string]fakeKey
	// Calls records every accepted operation in order, for parity comparison.
	Calls []string
}

type fakeKey struct {
	digest string
	id     string
}

// Fake failure modes, mapped from the server's problem types.
var (
	// ErrConflict is an Idempotency-Key reused with a different payload.
	ErrConflict = errors.New("cityneutral: idempotency key reused with a different payload")
	// ErrSessionFinalized is a write to a finalized session.
	ErrSessionFinalized = errors.New("cityneutral: session is finalized")
	// ErrNotFound is a read or write against an unknown neutral ID.
	ErrNotFound = errors.New("cityneutral: not found")
	// ErrMissingKey is a mutation with no Idempotency-Key.
	ErrMissingKey = errors.New("cityneutral: Idempotency-Key is required")
)

// NewFake returns a Fake bound to one credential's uploader and source.
func NewFake(uploader, sourceID, sourceKind string) *Fake {
	return &Fake{
		Uploader: uploader, SourceID: sourceID, SourceKind: sourceKind,
		runs: map[string]*Run{}, runIdx: map[string]string{},
		sessions: map[string]*Session{}, sessIdx: map[string]string{},
		records: map[string][]TranscriptRecord{}, text: map[string]string{},
		keys: map[string]fakeKey{},
	}
}

func (f *Fake) mint(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s_%016x", prefix, f.seq)
}

// take consumes a pending injected failure.
func (f *Fake) take() error {
	if f.Fail == nil {
		return nil
	}
	err := f.Fail
	f.Fail = nil
	return err
}

// replay resolves an idempotency key: it returns the prior id when the key and
// payload match, and refuses when the key was used for different bytes.
func (f *Fake) replay(key, digest string) (string, bool, error) {
	if key == "" {
		return "", false, ErrMissingKey
	}
	prior, ok := f.keys[key]
	if !ok {
		return "", false, nil
	}
	if prior.digest != digest {
		return "", false, fmt.Errorf("%w: key %s", ErrConflict, key)
	}
	return prior.id, true, nil
}

// UpsertRun implements API.
func (f *Fake) UpsertRun(_ context.Context, body Upsert, key string) (Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take(); err != nil {
		return Run{}, err
	}
	digest := payloadDigest(body)
	if id, hit, err := f.replay(key, digest); err != nil {
		return Run{}, err
	} else if hit {
		f.Calls = append(f.Calls, OpUpsertRun+" replay")
		return *f.runs[id], nil
	}
	idxKey := f.SourceID + "/" + body.SourceEntityID
	id, ok := f.runIdx[idxKey]
	if !ok {
		id = f.mint("run")
		f.runIdx[idxKey] = id
		f.runs[id] = &Run{
			ID: id, SourceRunID: body.SourceEntityID,
			SourceID: f.SourceID, SourceKind: f.SourceKind,
		}
	}
	run := f.runs[id]
	run.Status, run.Lifecycle, run.Epoch = body.Status, body.Lifecycle, body.Epoch
	run.Version = body.SourceVersion
	run.ETag = payloadDigest(*run)
	f.keys[key] = fakeKey{digest: digest, id: id}
	f.Calls = append(f.Calls, OpUpsertRun)
	return *run, nil
}

// GetRun implements API.
func (f *Fake) GetRun(_ context.Context, runTeamID string) (Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	run, ok := f.runs[runTeamID]
	if !ok {
		return Run{}, fmt.Errorf("%w: run %s", ErrNotFound, runTeamID)
	}
	f.Calls = append(f.Calls, OpGetRun)
	return *run, nil
}

// UpsertRunSession implements API.
func (f *Fake) UpsertRunSession(_ context.Context, runTeamID string, body Upsert, key string) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take(); err != nil {
		return Session{}, err
	}
	if _, ok := f.runs[runTeamID]; !ok {
		return Session{}, fmt.Errorf("%w: run %s", ErrNotFound, runTeamID)
	}
	digest := payloadDigest(body)
	if id, hit, err := f.replay(key, digest); err != nil {
		return Session{}, err
	} else if hit {
		f.Calls = append(f.Calls, OpUpsertRunSession+" replay")
		return *f.sessions[id], nil
	}
	idxKey := f.SourceID + "/" + runTeamID + "/" + body.SourceEntityID
	id, ok := f.sessIdx[idxKey]
	if !ok {
		id = f.mint("ses")
		f.sessIdx[idxKey] = id
		f.sessions[id] = &Session{
			ID: id, RunID: runTeamID, SourceSessionID: body.SourceEntityID,
			SourceID: f.SourceID, SourceKind: f.SourceKind,
		}
	}
	sess := f.sessions[id]
	if sess.Finalized {
		return Session{}, fmt.Errorf("%w: %s", ErrSessionFinalized, id)
	}
	sess.Status, sess.Lifecycle, sess.Epoch = body.Status, body.Lifecycle, body.Epoch
	sess.Version = body.SourceVersion
	sess.ETag = payloadDigest(*sess)
	f.keys[key] = fakeKey{digest: digest, id: id}
	f.Calls = append(f.Calls, OpUpsertRunSession)
	return *sess, nil
}

// GetSession implements API.
func (f *Fake) GetSession(_ context.Context, sessionTeamID string) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sess, ok := f.sessions[sessionTeamID]
	if !ok {
		return Session{}, fmt.Errorf("%w: session %s", ErrNotFound, sessionTeamID)
	}
	f.Calls = append(f.Calls, OpGetSession)
	return *sess, nil
}

// FinalizeSession implements API. Finalize is one-way and idempotent.
func (f *Fake) FinalizeSession(_ context.Context, sessionTeamID, key string) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take(); err != nil {
		return Session{}, err
	}
	sess, ok := f.sessions[sessionTeamID]
	if !ok {
		return Session{}, fmt.Errorf("%w: session %s", ErrNotFound, sessionTeamID)
	}
	if key == "" {
		return Session{}, ErrMissingKey
	}
	sess.Finalized = true
	sess.Lifecycle = LifecycleFinal
	sess.ETag = payloadDigest(*sess)
	f.Calls = append(f.Calls, OpFinalizeSession)
	return *sess, nil
}

// CreateTranscriptRecord implements API.
func (f *Fake) CreateTranscriptRecord(_ context.Context, sessionTeamID string, body TranscriptRecordIngest, key string) (TranscriptRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take(); err != nil {
		return TranscriptRecord{}, err
	}
	sess, ok := f.sessions[sessionTeamID]
	if !ok {
		return TranscriptRecord{}, fmt.Errorf("%w: session %s", ErrNotFound, sessionTeamID)
	}
	digest := payloadDigest(body)
	if id, hit, err := f.replay(key, digest); err != nil {
		return TranscriptRecord{}, err
	} else if hit {
		for _, r := range f.records[sessionTeamID] {
			if r.ID == id {
				f.Calls = append(f.Calls, OpCreateTranscriptRecord+" replay")
				return r, nil
			}
		}
	}
	if sess.Finalized {
		return TranscriptRecord{}, fmt.Errorf("%w: %s", ErrSessionFinalized, sessionTeamID)
	}
	id := f.mint("rec")
	status := "missing"
	if body.Text != "" {
		status = "available"
		f.text[id] = body.Text
	} else if body.ContentRef != "" {
		status = "missing"
	}
	rec := TranscriptRecord{
		ID: id, SessionID: sessionTeamID, SourceMessageID: body.SourceMessageID,
		SourceID: f.SourceID, SourceKind: f.SourceKind,
		Role: body.Role, Author: body.Author, OccurredAt: body.OccurredAt,
		Ordinal: body.Ordinal, Epoch: body.Epoch, Version: body.SourceVersion,
		ContentStatus: status,
	}
	rec.ETag = payloadDigest(rec)
	f.records[sessionTeamID] = append(f.records[sessionTeamID], rec)
	f.keys[key] = fakeKey{digest: digest, id: id}
	f.Calls = append(f.Calls, OpCreateTranscriptRecord)
	return rec, nil
}

// ListTranscriptRecords implements API.
func (f *Fake) ListTranscriptRecords(_ context.Context, sessionTeamID string) ([]TranscriptRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sessions[sessionTeamID]; !ok {
		return nil, fmt.Errorf("%w: session %s", ErrNotFound, sessionTeamID)
	}
	f.Calls = append(f.Calls, OpListTranscriptRecords)
	return append([]TranscriptRecord(nil), f.records[sessionTeamID]...), nil
}

// GetSessionTranscript implements API.
func (f *Fake) GetSessionTranscript(_ context.Context, sessionTeamID string) ([]TranscriptItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sessions[sessionTeamID]; !ok {
		return nil, fmt.Errorf("%w: session %s", ErrNotFound, sessionTeamID)
	}
	items := make([]TranscriptItem, 0, len(f.records[sessionTeamID]))
	for _, r := range f.records[sessionTeamID] {
		items = append(items, TranscriptItem{
			RecordID: r.ID, SourceMessageID: r.SourceMessageID, Role: r.Role,
			Author: r.Author, Ordinal: r.Ordinal, ContentStatus: r.ContentStatus,
			Text: f.text[r.ID],
		})
	}
	f.Calls = append(f.Calls, OpGetSessionTranscript)
	return items, nil
}
