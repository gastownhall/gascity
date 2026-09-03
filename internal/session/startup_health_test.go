package session

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// erroringStore wraps a beads.Store and forces selected methods to fail,
// isolating how the startup-health Store methods propagate a lower-layer
// error (as opposed to misclassifying it as "not found" or swallowing it).
type erroringStore struct {
	beads.Store
	createErr error
	setErr    error
	listErr   error
}

func (e *erroringStore) Create(b beads.Bead) (beads.Bead, error) {
	if e.createErr != nil {
		return beads.Bead{}, e.createErr
	}
	return e.Store.Create(b)
}

func (e *erroringStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if e.setErr != nil {
		return e.setErr
	}
	return e.Store.SetMetadataBatch(id, kvs)
}

func (e *erroringStore) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	if e.listErr != nil {
		return nil, e.listErr
	}
	return e.Store.ListByMetadata(filters, limit, opts...)
}

// startupHealthBeadFixture builds a startup-health-episode bead for the given
// session name, merging in any additional metadata. Callers pass the result
// to recordingStore or a *beads.MemStore seed.
func startupHealthBeadFixture(id, sessionName string, meta map[string]string) beads.Bead {
	m := map[string]string{}
	for k, v := range meta {
		m[k] = v
	}
	m[StartupHealthSessionNameMetadataKey] = sessionName
	return beads.Bead{
		ID:       id,
		Type:     StartupHealthEpisodeType,
		Status:   "open",
		Metadata: m,
	}
}

func TestStartupHealthEpisodeFromMetadataProjectsVerbatim(t *testing.T) {
	firstFailure := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	lastFailure := time.Date(2026, 8, 20, 10, 30, 0, 0, time.UTC)
	quarantinedUntil := time.Date(2026, 8, 20, 10, 35, 0, 0, time.UTC)

	tests := []struct {
		name string
		meta map[string]string
		want StartupHealthEpisode
	}{
		{
			name: "nil metadata projects the zero value",
			meta: nil,
			want: StartupHealthEpisode{},
		},
		{
			name: "empty metadata projects the zero value",
			meta: map[string]string{},
			want: StartupHealthEpisode{},
		},
		{
			name: "full metadata projects every field verbatim",
			meta: map[string]string{
				StartupHealthSessionNameMetadataKey:      "chaos-worker-1",
				StartupHealthConsecutiveMetadataKey:      "3",
				StartupHealthFirstFailureMetadataKey:     firstFailure.Format(time.RFC3339),
				StartupHealthLastFailureMetadataKey:      lastFailure.Format(time.RFC3339),
				StartupHealthLastDetailMetadataKey:       "exit status 1",
				StartupHealthAlertMetadataKey:            string(AlertDispositionPending),
				StartupHealthQuarantinedUntilMetadataKey: quarantinedUntil.Format(time.RFC3339),
			},
			want: StartupHealthEpisode{
				SessionName:      "chaos-worker-1",
				ConsecutiveCount: 3,
				FirstFailureAt:   firstFailure,
				LastFailureAt:    lastFailure,
				LastDetail:       "exit status 1",
				AlertDisposition: AlertDispositionPending,
				QuarantinedUntil: quarantinedUntil,
			},
		},
		{
			name: "malformed numeric and time fields fall back to the zero value, strings stay verbatim",
			meta: map[string]string{
				StartupHealthSessionNameMetadataKey:      "chaos-worker-2",
				StartupHealthConsecutiveMetadataKey:      "not-a-number",
				StartupHealthFirstFailureMetadataKey:     "not-a-time",
				StartupHealthLastFailureMetadataKey:      "",
				StartupHealthLastDetailMetadataKey:       "garbled\x00detail",
				StartupHealthAlertMetadataKey:            "unrecognized-disposition",
				StartupHealthQuarantinedUntilMetadataKey: "also-not-a-time",
			},
			want: StartupHealthEpisode{
				SessionName:      "chaos-worker-2",
				ConsecutiveCount: 0,
				FirstFailureAt:   time.Time{},
				LastFailureAt:    time.Time{},
				LastDetail:       "garbled\x00detail",
				AlertDisposition: AlertDisposition("unrecognized-disposition"),
				QuarantinedUntil: time.Time{},
			},
		},
		{
			name: "unrelated keys are ignored",
			meta: map[string]string{"state": "asleep", "wake_attempts": "9"},
			want: StartupHealthEpisode{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StartupHealthEpisodeFromMetadata(tt.meta); got != tt.want {
				t.Errorf("StartupHealthEpisodeFromMetadata(%+v) = %+v, want %+v", tt.meta, got, tt.want)
			}
		})
	}
}

func TestRecordStartupFailureBelowThreshold(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	prior := StartupHealthEpisode{SessionName: "w", ConsecutiveCount: 1}
	got := RecordStartupFailure(prior, "boom", now, 5, 5*time.Minute)
	if got.ConsecutiveCount != 2 {
		t.Errorf("ConsecutiveCount = %d, want 2", got.ConsecutiveCount)
	}
	if !got.QuarantinedUntil.IsZero() {
		t.Errorf("QuarantinedUntil = %v, want zero (below threshold)", got.QuarantinedUntil)
	}
	if got.SessionName != "w" {
		t.Errorf("SessionName = %q, want %q", got.SessionName, "w")
	}
	if got.LastDetail != "boom" {
		t.Errorf("LastDetail = %q, want %q", got.LastDetail, "boom")
	}
	if !got.LastFailureAt.Equal(now) {
		t.Errorf("LastFailureAt = %v, want %v", got.LastFailureAt, now)
	}
}

func TestRecordStartupFailurePreservesFirstFailureAcrossAccrual(t *testing.T) {
	first := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	prior := StartupHealthEpisode{SessionName: "w", ConsecutiveCount: 1, FirstFailureAt: first}
	later := first.Add(10 * time.Minute)
	got := RecordStartupFailure(prior, "boom again", later, 5, 5*time.Minute)
	if !got.FirstFailureAt.Equal(first) {
		t.Errorf("FirstFailureAt = %v, want unchanged %v", got.FirstFailureAt, first)
	}
	if !got.LastFailureAt.Equal(later) {
		t.Errorf("LastFailureAt = %v, want %v", got.LastFailureAt, later)
	}
}

func TestRecordStartupFailureAtThreshold(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	prior := StartupHealthEpisode{SessionName: "w", ConsecutiveCount: 4}
	got := RecordStartupFailure(prior, "boom", now, 5, 5*time.Minute)
	if got.ConsecutiveCount != 5 {
		t.Fatalf("ConsecutiveCount = %d, want 5", got.ConsecutiveCount)
	}
	want := now.Add(5 * time.Minute)
	if !got.QuarantinedUntil.Equal(want) {
		t.Errorf("QuarantinedUntil = %v, want %v", got.QuarantinedUntil, want)
	}
}

func TestRecordStartupFailureBelowThresholdLeavesQuarantineUnset(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	prior := StartupHealthEpisode{SessionName: "w"}
	for i := 0; i < 4; i++ { //nolint:staticcheck // SA4008 false positive: RecordStartupFailure is a RED-phase panic stub (ga-o04bfr.1.1); staticcheck sees the body as unconditionally terminating and misreports i++ as dead. Resolves at GREEN.
		prior = RecordStartupFailure(prior, "boom", now, 5, 5*time.Minute)
		if !prior.QuarantinedUntil.IsZero() {
			t.Fatalf("iteration %d: QuarantinedUntil = %v, want zero below threshold", i, prior.QuarantinedUntil)
		}
	}
	if prior.ConsecutiveCount != 4 {
		t.Fatalf("ConsecutiveCount = %d, want 4", prior.ConsecutiveCount)
	}
}

func TestRecordStartupFailureSetsAlertDispositionPendingOnQuarantine(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	prior := StartupHealthEpisode{SessionName: "w", ConsecutiveCount: 4}
	got := RecordStartupFailure(prior, "boom", now, 5, 5*time.Minute)
	if got.AlertDisposition != AlertDispositionPending {
		t.Errorf("AlertDisposition = %q, want %q", got.AlertDisposition, AlertDispositionPending)
	}
}

func TestRecordStartupFailureAlertDispositionNeverRegressesFromSent(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	prior := StartupHealthEpisode{SessionName: "w", ConsecutiveCount: 4, AlertDisposition: AlertDispositionSent}
	got := RecordStartupFailure(prior, "boom", now, 5, 5*time.Minute)
	if got.AlertDisposition != AlertDispositionSent {
		t.Errorf("AlertDisposition = %q, want %q (must not regress from sent)", got.AlertDisposition, AlertDispositionSent)
	}
}

func TestRecordStartupFailureTruncatesDetail(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	huge := strings.Repeat("boom", 200) // 800 runes
	got := RecordStartupFailure(StartupHealthEpisode{SessionName: "w"}, huge, now, 5, 5*time.Minute)
	if n := len([]rune(got.LastDetail)); n != startupHealthLastDetailMaxRunes {
		t.Errorf("len(LastDetail) = %d, want %d", n, startupHealthLastDetailMaxRunes)
	}
}

func TestClearStartupHealthEpisodeResetsToZeroValue(t *testing.T) {
	got := ClearStartupHealthEpisode("w")
	want := StartupHealthEpisode{SessionName: "w"}
	if got != want {
		t.Errorf("ClearStartupHealthEpisode(%q) = %+v, want %+v", "w", got, want)
	}
}

func TestStoreSaveAndLoadStartupHealthEpisodeRoundTrip(t *testing.T) {
	mem := beads.NewMemStore()
	s := NewStore(beads.SessionStore{Store: mem})
	ep := StartupHealthEpisode{
		SessionName:      "chaos-worker-1",
		ConsecutiveCount: 2,
		FirstFailureAt:   time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
		LastFailureAt:    time.Date(2026, 8, 20, 12, 5, 0, 0, time.UTC),
		LastDetail:       "exit status 1",
		AlertDisposition: AlertDispositionNone,
	}
	if err := s.SaveStartupHealthEpisode(ep); err != nil {
		t.Fatalf("SaveStartupHealthEpisode: %v", err)
	}
	got, err := s.LoadStartupHealthEpisode("chaos-worker-1")
	if err != nil {
		t.Fatalf("LoadStartupHealthEpisode: %v", err)
	}
	if got != ep {
		t.Errorf("round trip = %+v, want %+v", got, ep)
	}
}

func TestStoreLoadStartupHealthEpisodeNotFoundReturnsZeroValue(t *testing.T) {
	s := NewStore(beads.SessionStore{Store: beads.NewMemStore()})
	got, err := s.LoadStartupHealthEpisode("never-seen")
	if err != nil {
		t.Fatalf("LoadStartupHealthEpisode: %v", err)
	}
	if got != (StartupHealthEpisode{}) {
		t.Errorf("LoadStartupHealthEpisode(not found) = %+v, want zero value", got)
	}
}

func TestStoreSaveStartupHealthEpisodeUpsertsNoDuplicate(t *testing.T) {
	mem := beads.NewMemStore()
	s := NewStore(beads.SessionStore{Store: mem})
	first := StartupHealthEpisode{SessionName: "w", ConsecutiveCount: 1}
	second := StartupHealthEpisode{SessionName: "w", ConsecutiveCount: 2} //nolint:staticcheck // SA4006 false positive: SaveStartupHealthEpisode is a RED-phase panic stub (ga-o04bfr.1.1); staticcheck treats second's later use as unreachable past the panic. Resolves at GREEN.
	if err := s.SaveStartupHealthEpisode(first); err != nil {
		t.Fatalf("first SaveStartupHealthEpisode: %v", err)
	}
	if err := s.SaveStartupHealthEpisode(second); err != nil {
		t.Fatalf("second SaveStartupHealthEpisode: %v", err)
	}
	matches, err := mem.ListByMetadata(map[string]string{StartupHealthSessionNameMetadataKey: "w"}, 0)
	if err != nil {
		t.Fatalf("ListByMetadata: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("len(matches) = %d, want 1 (upsert must not duplicate)", len(matches))
	}
	got, err := s.LoadStartupHealthEpisode("w")
	if err != nil {
		t.Fatalf("LoadStartupHealthEpisode: %v", err)
	}
	if got != second {
		t.Errorf("LoadStartupHealthEpisode after second save = %+v, want %+v (second save must win)", got, second)
	}
}

func TestStoreLoadStartupHealthEpisodePropagatesStoreError(t *testing.T) {
	boom := errors.New("boom")
	s := NewStore(beads.SessionStore{Store: &erroringStore{Store: beads.NewMemStore(), listErr: boom}})
	_, err := s.LoadStartupHealthEpisode("w")
	if !errors.Is(err, boom) {
		t.Errorf("LoadStartupHealthEpisode error = %v, want wrapped %v", err, boom)
	}
}

func TestStoreSaveStartupHealthEpisodePropagatesCreateError(t *testing.T) {
	boom := errors.New("boom")
	s := NewStore(beads.SessionStore{Store: &erroringStore{Store: beads.NewMemStore(), createErr: boom}})
	err := s.SaveStartupHealthEpisode(StartupHealthEpisode{SessionName: "w"})
	if !errors.Is(err, boom) {
		t.Errorf("SaveStartupHealthEpisode (create) error = %v, want wrapped %v", err, boom)
	}
}

func TestStoreSaveStartupHealthEpisodePropagatesUpdateError(t *testing.T) {
	mem := beads.NewMemStore()
	clean := NewStore(beads.SessionStore{Store: mem})
	if err := clean.SaveStartupHealthEpisode(StartupHealthEpisode{SessionName: "w"}); err != nil {
		t.Fatalf("seeding initial episode: %v", err)
	}
	boom := errors.New("boom")
	s := NewStore(beads.SessionStore{Store: &erroringStore{Store: mem, setErr: boom}})
	err := s.SaveStartupHealthEpisode(StartupHealthEpisode{SessionName: "w", ConsecutiveCount: 1})
	if !errors.Is(err, boom) {
		t.Errorf("SaveStartupHealthEpisode (update) error = %v, want wrapped %v", err, boom)
	}
}

func TestStoreStartupHealthEpisodeReadOnlyLoadEmitsNoMutatingOps(t *testing.T) {
	fixture := startupHealthBeadFixture("sh-1", "w", nil)
	s, rec := recordingStore(t, fixture) //nolint:staticcheck // SA4006 false positive: LoadStartupHealthEpisode is a RED-phase panic stub (ga-o04bfr.1.1); staticcheck treats rec's later use as unreachable past the panic. Resolves at GREEN.
	if _, err := s.LoadStartupHealthEpisode("w"); err != nil {
		t.Fatalf("LoadStartupHealthEpisode: %v", err)
	}
	if ops := opsOf(rec.Calls()); len(ops) != 0 {
		t.Errorf("LoadStartupHealthEpisode recorded mutating ops %v, want none", ops)
	}
}

func TestStoreSavedStartupHealthEpisodeExcludedFromSessionList(t *testing.T) {
	mem := beads.NewMemStore()
	s := NewStore(beads.SessionStore{Store: mem})
	if err := s.SaveStartupHealthEpisode(StartupHealthEpisode{SessionName: "w"}); err != nil {
		t.Fatalf("SaveStartupHealthEpisode: %v", err)
	}
	sessionBead := beads.Bead{
		Type:     "session",
		Status:   "open",
		Labels:   []string{LabelSession},
		Metadata: map[string]string{"session_name": "w", "alias": "w", "state": "active"},
	}
	created, err := mem.Create(sessionBead)
	if err != nil {
		t.Fatalf("creating session bead fixture: %v", err)
	}
	infos, err := s.List("", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 1 || infos[0].ID != created.ID {
		t.Fatalf("List() = %+v, want exactly the session bead %q (never the startup-health episode)", infos, created.ID)
	}
}

// recordingStartupHealthCacheStore records List and CachedList calls, serving
// CachedList from the embedded store's List (NOT the recorded override) so a
// cache hit leaves listCalls at zero, mirroring recordingCacheStore's role for
// the two-leg session union tier tests in list_all_characterization_test.go.
type recordingStartupHealthCacheStore struct {
	beads.Store
	listCalls   []beads.ListQuery
	cachedCalls []beads.ListQuery
	cacheHit    bool
}

func (s *recordingStartupHealthCacheStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.listCalls = append(s.listCalls, q)
	return s.Store.List(q)
}

func (s *recordingStartupHealthCacheStore) CachedList(q beads.ListQuery) ([]beads.Bead, bool) {
	s.cachedCalls = append(s.cachedCalls, q)
	if !s.cacheHit {
		return nil, false
	}
	rows, err := s.Store.List(q)
	if err != nil {
		return nil, false
	}
	return rows, true
}

func TestListStartupHealthEpisodesCacheFirstHitSkipsBackingList(t *testing.T) {
	mem := beads.NewMemStore()
	seed := NewStore(beads.SessionStore{Store: mem})
	ep := StartupHealthEpisode{
		SessionName:      "gc-1",
		ConsecutiveCount: 3,
		FirstFailureAt:   time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
		LastFailureAt:    time.Date(2026, 8, 20, 12, 5, 0, 0, time.UTC),
		QuarantinedUntil: time.Date(2026, 8, 20, 13, 0, 0, 0, time.UTC),
	}
	if err := seed.SaveStartupHealthEpisode(ep); err != nil {
		t.Fatalf("seed SaveStartupHealthEpisode: %v", err)
	}

	rec := &recordingStartupHealthCacheStore{Store: mem, cacheHit: true}
	s := NewStore(beads.SessionStore{Store: rec})

	episodes, err := s.ListStartupHealthEpisodesCacheFirst()
	if err != nil {
		t.Fatalf("ListStartupHealthEpisodesCacheFirst: %v", err)
	}
	if len(rec.listCalls) != 0 {
		t.Errorf("cache hit must not call backing List, got %d calls", len(rec.listCalls))
	}
	if len(rec.cachedCalls) != 1 {
		t.Fatalf("want 1 CachedList peek, got %d", len(rec.cachedCalls))
	}
	if rec.cachedCalls[0].IncludeClosed {
		t.Errorf("cache peek must not set IncludeClosed (the real CachingStore declines closed-inclusive queries), got true")
	}
	if len(episodes) != 1 || episodes[0] != ep {
		t.Fatalf("episodes = %+v, want [%+v]", episodes, ep)
	}
}

func TestListStartupHealthEpisodesCacheFirstMissFallsThroughToBackingList(t *testing.T) {
	mem := beads.NewMemStore()
	seed := NewStore(beads.SessionStore{Store: mem})
	ep := StartupHealthEpisode{SessionName: "gc-1", ConsecutiveCount: 3}
	if err := seed.SaveStartupHealthEpisode(ep); err != nil {
		t.Fatalf("seed SaveStartupHealthEpisode: %v", err)
	}

	rec := &recordingStartupHealthCacheStore{Store: mem, cacheHit: false}
	s := NewStore(beads.SessionStore{Store: rec})

	episodes, err := s.ListStartupHealthEpisodesCacheFirst()
	if err != nil {
		t.Fatalf("ListStartupHealthEpisodesCacheFirst: %v", err)
	}
	if len(rec.cachedCalls) != 1 {
		t.Fatalf("want 1 CachedList peek attempted before falling through, got %d", len(rec.cachedCalls))
	}
	if len(rec.listCalls) != 1 {
		t.Fatalf("cache miss must fall through to backing List, got %d calls", len(rec.listCalls))
	}
	if !rec.listCalls[0].IncludeClosed {
		t.Errorf("fallback must match ListStartupHealthEpisodes and query with IncludeClosed=true")
	}
	if len(episodes) != 1 || episodes[0].SessionName != "gc-1" || episodes[0].ConsecutiveCount != 3 {
		t.Fatalf("episodes = %+v, want one gc-1 episode with count 3", episodes)
	}
}

func TestListStartupHealthEpisodesCacheFirstNoCacheCapabilityFallsThrough(t *testing.T) {
	mem := beads.NewMemStore()
	s := NewStore(beads.SessionStore{Store: mem})
	if err := s.SaveStartupHealthEpisode(StartupHealthEpisode{SessionName: "gc-1", ConsecutiveCount: 1}); err != nil {
		t.Fatalf("seed SaveStartupHealthEpisode: %v", err)
	}

	episodes, err := s.ListStartupHealthEpisodesCacheFirst()
	if err != nil {
		t.Fatalf("ListStartupHealthEpisodesCacheFirst: %v", err)
	}
	if len(episodes) != 1 || episodes[0].SessionName != "gc-1" {
		t.Fatalf("episodes = %+v, want one gc-1 episode (a store without CachedList must fall back cleanly)", episodes)
	}
}
