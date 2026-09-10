package main

import (
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// twoCallPoolIdentifierLocker is a deterministic two-creator lock harness.
// The first callback owns its identifiers until it returns. The second call
// reports whether its requested set intersects that ownership before either
// waiting or entering its callback. Tests can therefore prove that the exact
// derived runtime name participates in exclusion without using elapsed time as
// a scheduler proxy.
type twoCallPoolIdentifierLocker struct {
	mu              sync.Mutex
	held            map[string]bool
	calls           [][]string
	firstDone       chan struct{}
	secondConflicts chan bool
}

func newTwoCallPoolIdentifierLocker() *twoCallPoolIdentifierLocker {
	return &twoCallPoolIdentifierLocker{
		held:            make(map[string]bool),
		firstDone:       make(chan struct{}),
		secondConflicts: make(chan bool, 1),
	}
}

func (l *twoCallPoolIdentifierLocker) withLocks(_ string, identifiers []string, fn func() error) error {
	normalized := make([]string, 0, len(identifiers))
	seen := make(map[string]bool, len(identifiers))
	for _, identifier := range identifiers {
		identifier = strings.TrimSpace(identifier)
		if identifier == "" || seen[identifier] {
			continue
		}
		seen[identifier] = true
		normalized = append(normalized, identifier)
	}
	sort.Strings(normalized)

	l.mu.Lock()
	call := len(l.calls)
	l.calls = append(l.calls, append([]string(nil), normalized...))
	conflicts := false
	for _, identifier := range normalized {
		if l.held[identifier] {
			conflicts = true
			break
		}
	}
	if call == 0 {
		for _, identifier := range normalized {
			l.held[identifier] = true
		}
	}
	l.mu.Unlock()

	if call == 1 {
		l.secondConflicts <- conflicts
		if conflicts {
			<-l.firstDone
		}
		l.mu.Lock()
		for _, identifier := range normalized {
			l.held[identifier] = true
		}
		l.mu.Unlock()
	}

	err := fn()

	l.mu.Lock()
	for _, identifier := range normalized {
		delete(l.held, identifier)
	}
	l.mu.Unlock()
	if call == 0 {
		close(l.firstDone)
	}
	return err
}

func (l *twoCallPoolIdentifierLocker) snapshotCalls() [][]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([][]string, len(l.calls))
	for i := range l.calls {
		out[i] = append([]string(nil), l.calls[i]...)
	}
	return out
}

type guardedPoolCreateSpec struct {
	agent             *config.Agent
	template          string
	qualifiedInstance string
	slot              int
}

type toggleListFailStore struct {
	beads.Store
	fail bool
}

func (s *toggleListFailStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if s.fail {
		return nil, errors.New("cross-store list failed")
	}
	return s.Store.List(query)
}

func seedGuardedPoolSessionHolder(
	t *testing.T,
	store beads.Store,
	title, agentName, alias, sessionName string,
) sessionpkg.Info {
	t.Helper()
	bead, err := store.Create(beads.Bead{
		Title:  title,
		Status: "open",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:" + agentName},
		Metadata: map[string]string{
			"template":       agentName,
			"agent_name":     agentName,
			"alias":          alias,
			"session_name":   sessionName,
			"state":          string(sessionpkg.StateAwake),
			"session_origin": "manual",
			"manual_session": "true",
		},
	})
	if err != nil {
		t.Fatalf("seed foreign session holder: %v", err)
	}
	infos, err := sessionFrontDoor(store).ListAll(sessionpkg.ListAllOptions{})
	if err != nil {
		t.Fatalf("read seeded foreign session holder: %v", err)
	}
	for _, info := range infos {
		if info.ID == bead.ID {
			return info
		}
	}
	t.Fatalf("seeded foreign session holder %s was not projected", bead.ID)
	return sessionpkg.Info{}
}

func primeGuardedPoolCrossStoreCensus(
	t *testing.T,
	bp *agentBuildParams,
	rigStores map[string]beads.Store,
) {
	t.Helper()
	infos, err := collectAllOpenSessionInfos(bp.cityPath, bp.city, bp.beadStore, rigStores, nil)
	if err != nil {
		t.Fatalf("initial complete session census: %v", err)
	}
	bp.sessionBeads = newSessionBeadSnapshotFromInfos(nil)
	bp.sessionOccupancyInfos = infos
	bp.sessionSnapshotCompletenessKnown = true
	bp.sessionSnapshotComplete = true
	bp.sessionCensusRigStores = cloneSessionCensusRigStores(rigStores)
}

func assertExactRuntimeNameSerializesPoolCreates(
	t *testing.T,
	bp *agentBuildParams,
	store *blockingPoolCreateStore,
	locker *twoCallPoolIdentifierLocker,
	runtimeName string,
	firstSpec, secondSpec guardedPoolCreateSpec,
) {
	t.Helper()
	type result struct {
		info sessionpkg.Info
		err  error
	}
	firstResult := make(chan result, 1)
	secondResult := make(chan result, 1)
	create := func(spec guardedPoolCreateSpec, out chan<- result) {
		info, err := createPoolSessionBeadWithGuardedAliasUsingLock(
			bp,
			spec.agent,
			spec.template,
			spec.qualifiedInstance,
			spec.slot,
			nil,
			locker.withLocks,
		)
		out <- result{info: info, err: err}
	}

	go create(firstSpec, firstResult)
	select {
	case <-store.firstCreateStarted:
	case <-time.After(time.Second):
		t.Fatal("first pool create did not reach the store")
	}

	go create(secondSpec, secondResult)
	var conflicts bool
	select {
	case conflicts = <-locker.secondConflicts:
	case <-time.After(time.Second):
		close(store.releaseFirstCreate)
		t.Fatal("second pool create did not reach the identifier locker")
	}
	if !conflicts {
		// Release both callbacks before failing so the test cannot strand either
		// goroutine if the exact runtime identifier was omitted from the lock set.
		close(store.releaseFirstCreate)
		close(store.releaseSecondCreate)
		<-firstResult
		<-secondResult
		t.Fatalf("two creators of runtime name %q had disjoint identifier locks", runtimeName)
	}

	close(store.releaseFirstCreate)
	first := <-firstResult
	var second result
	select {
	case second = <-secondResult:
	case <-time.After(time.Second):
		// If the post-lock availability recheck regresses, creator 2 reaches
		// Create and blocks in the store instead of returning the expected
		// collision. Release and drain it before failing so this regression is
		// reported promptly rather than at the package timeout.
		close(store.releaseSecondCreate)
		second = <-secondResult
		t.Fatalf("second creator reached the store after waiting for runtime-name lock: info=%#v err=%v", second.info, second.err)
	}
	if first.err != nil {
		t.Fatalf("first create: %v", first.err)
	}
	if got := first.info.SessionNameMetadata; got != runtimeName {
		t.Fatalf("first session_name = %q, want %q", got, runtimeName)
	}
	if !errors.Is(second.err, errPoolSessionNameUnavailable) {
		t.Fatalf("second create error = %v, want errPoolSessionNameUnavailable", second.err)
	}
	if second.info.ID != "" {
		t.Fatalf("refused second create returned session %#v", second.info)
	}

	created, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("load session beads: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("session beads = %#v, want exactly one runtime-name owner", created)
	}
	for call, identifiers := range locker.snapshotCalls() {
		if !containsString(identifiers, runtimeName) {
			t.Fatalf("lock call %d identifiers = %v, want exact runtime name %q", call+1, identifiers, runtimeName)
		}
	}
}

func TestCreatePoolSessionBeadWithGuardedAlias_TransientExactNameRace(t *testing.T) {
	store := newBlockingPoolCreateStore("")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(2),
		}},
	}
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, io.Discard)
	bp.sessionBeads = newSessionBeadSnapshot(nil)
	locker := newTwoCallPoolIdentifierLocker()

	// Qualified identities are not a bijection under the runtime's tmux-safe
	// encoding: both of these spellings derive "rig--worker-1-pool". Their raw
	// alias locks differ, so only the exact derived runtime-name lock joins the
	// two create paths.
	assertExactRuntimeNameSerializesPoolCreates(t, bp, store, locker, "rig--worker-1-pool",
		guardedPoolCreateSpec{agent: &cfg.Agents[0], template: "worker", qualifiedInstance: "rig/worker-1", slot: 1},
		guardedPoolCreateSpec{agent: &cfg.Agents[0], template: "worker", qualifiedInstance: "rig--worker-1", slot: 1},
	)
}

func TestCreatePoolSessionBeadWithGuardedAlias_AliasedSlot2ExactNameRace(t *testing.T) {
	store := newBlockingPoolCreateStore("")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{
				Name:              "worker-a",
				StartCommand:      "true",
				MaxActiveSessions: intPtr(2),
				TmuxAlias:         "crew",
			},
			{
				Name:              "worker-b",
				StartCommand:      "true",
				MaxActiveSessions: intPtr(2),
				TmuxAlias:         "crew-2",
			},
		},
	}
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, io.Discard)
	bp.sessionBeads = newSessionBeadSnapshot(nil)
	locker := newTwoCallPoolIdentifierLocker()

	// Slot 2 of base alias "crew" and slot 1 of base alias "crew-2" both
	// derive the exact runtime name "crew-2". Their base and qualified-alias
	// lock sets differ, so the derived-name lock is the only common fence.
	assertExactRuntimeNameSerializesPoolCreates(t, bp, store, locker, "crew-2",
		guardedPoolCreateSpec{agent: &cfg.Agents[0], template: "worker-a", qualifiedInstance: "worker-a-2", slot: 2},
		guardedPoolCreateSpec{agent: &cfg.Agents[1], template: "worker-b", qualifiedInstance: "worker-b-1", slot: 1},
	)
}

func TestCreatePoolSessionBeadWithGuardedAlias_AliasAvailabilityErrorNeverCreates(t *testing.T) {
	mem := beads.NewMemStore()
	store := listFailStore{Store: mem}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(2),
			TmuxAlias:         "crew",
		}},
	}
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, io.Discard)
	bp.sessionBeads = newSessionBeadSnapshot(nil)

	info, err := createPoolSessionBeadWithGuardedAlias(bp, &cfg.Agents[0], "worker", "worker-1", 1, nil)
	if err == nil || !strings.Contains(err.Error(), "checking locked pool availability") || !strings.Contains(err.Error(), "session census leg") || !strings.Contains(err.Error(), "list failed") {
		t.Fatalf("createPoolSessionBeadWithGuardedAlias error = %v, want wrapped complete-census availability failure", err)
	}
	if info.ID != "" {
		t.Fatalf("availability-refused create returned session %#v", info)
	}
	created, listErr := mem.ListByLabel(sessionBeadLabel, 0)
	if listErr != nil {
		t.Fatalf("listing session beads: %v", listErr)
	}
	if len(created) != 0 {
		t.Fatalf("session beads = %#v, want none after alias availability error", created)
	}
}

func TestCreatePoolSessionBeadWithGuardedAlias_ProvenAliasCollisionDefersAlias(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "manual alias holder",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:rig/manual"},
		Metadata: map[string]string{
			"template":       "rig/manual",
			"agent_name":     "rig/manual",
			"alias":          "rig/furiosa",
			"session_name":   "manual-furiosa",
			"state":          "awake",
			"session_origin": "manual",
			"manual_session": "true",
		},
	}); err != nil {
		t.Fatalf("seed alias holder: %v", err)
	}

	maxSessions := 2
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			Dir:               "rig",
			StartCommand:      "true",
			MaxActiveSessions: &maxSessions,
			NamepoolNames:     []string{"furiosa", "nux"},
		}},
	}
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, io.Discard)
	bp.sessionBeads = newSessionBeadSnapshotFromInfos(nil)
	bp.sessionOccupancyInfos = bp.sessionBeads.OpenInfos()
	bp.sessionSnapshotCompletenessKnown = true
	bp.sessionSnapshotComplete = true

	_, qualifiedInstance, slot := poolDesiredRequestIdentity(&cfg.Agents[0], 1)
	if qualifiedInstance != "rig/furiosa" || slot != 1 {
		t.Fatalf("namepool identity = %q slot %d, want rig/furiosa slot 1", qualifiedInstance, slot)
	}
	expectedRuntimeName := poolRuntimeSessionName(cfg, qualifiedInstance, cfg.Agents[0].QualifiedName(), false)
	if expectedRuntimeName == qualifiedInstance {
		t.Fatalf("fixture runtime name %q must differ from colliding public alias %q", expectedRuntimeName, qualifiedInstance)
	}

	created, err := createPoolSessionBeadWithGuardedAlias(
		bp,
		&cfg.Agents[0],
		cfg.Agents[0].QualifiedName(),
		qualifiedInstance,
		slot,
		nil,
	)
	if err != nil {
		t.Fatalf("guarded create with proven public-alias collision: %v", err)
	}
	if created.ID == "" {
		t.Fatal("guarded alias-deferred create returned no session")
	}
	if created.Alias != "" {
		t.Fatalf("created alias = %q, want deferred/empty while rig/furiosa is held", created.Alias)
	}
	if created.SessionNameMetadata != expectedRuntimeName {
		t.Fatalf("created session_name = %q, want exact free runtime name %q", created.SessionNameMetadata, expectedRuntimeName)
	}

	rows, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("load session beads: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("session beads = %#v, want holder plus one alias-deferred pool row", rows)
	}
	if got := bp.sessionBeads.OpenInfos(); len(got) != 1 || got[0].ID != created.ID || got[0].Alias != "" {
		t.Fatalf("primary writeback = %#v, want exactly the alias-deferred create", got)
	}
}

func TestCreatePoolSessionBeadWithGuardedAlias_ForeignAliasCollisionDefersAlias(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := t.TempDir()
	primary := beads.NewMemStore()
	foreign := beads.NewMemStore()
	maxSessions := 2
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "rig", Path: rigPath}},
		Agents: []config.Agent{{
			Name:              "worker",
			Dir:               "rig",
			StartCommand:      "true",
			MaxActiveSessions: &maxSessions,
			NamepoolNames:     []string{"furiosa", "nux"},
		}},
	}
	holder := seedGuardedPoolSessionHolder(t, foreign, "foreign alias holder", "rig/manual", "rig/furiosa", "manual-furiosa")
	bp := newAgentBuildParams("test-city", cityPath, cfg, runtime.NewFake(), time.Now().UTC(), primary, io.Discard)
	primeGuardedPoolCrossStoreCensus(t, bp, map[string]beads.Store{"rig": foreign})

	_, qualifiedInstance, slot := poolDesiredRequestIdentity(&cfg.Agents[0], 1)
	expectedRuntimeName := poolRuntimeSessionName(cfg, qualifiedInstance, cfg.Agents[0].QualifiedName(), false)
	created, err := createPoolSessionBeadWithGuardedAlias(
		bp,
		&cfg.Agents[0],
		cfg.Agents[0].QualifiedName(),
		qualifiedInstance,
		slot,
		nil,
	)
	if err != nil {
		t.Fatalf("guarded create with foreign public-alias collision: %v", err)
	}
	if created.ID == "" || created.Alias != "" {
		t.Fatalf("created session = %#v, want one alias-deferred primary row", created)
	}
	if created.SessionNameMetadata != expectedRuntimeName {
		t.Fatalf("created session_name = %q, want %q", created.SessionNameMetadata, expectedRuntimeName)
	}
	if got := bp.sessionBeads.OpenInfos(); len(got) != 1 || got[0].ID != created.ID {
		t.Fatalf("primary writeback = %#v, want exactly created row %s", got, created.ID)
	}
	foreignInfos, err := sessionFrontDoor(foreign).ListAll(sessionpkg.ListAllOptions{})
	if err != nil || len(foreignInfos) != 1 || foreignInfos[0].ID != holder.ID || foreignInfos[0].Alias != "rig/furiosa" {
		t.Fatalf("foreign holder changed: infos=%#v err=%v", foreignInfos, err)
	}
}

func TestCreatePoolSessionBeadWithGuardedAlias_LateForeignExactNameHolderBlocksCreate(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := t.TempDir()
	primary := beads.NewMemStore()
	foreign := beads.NewMemStore()
	maxSessions := 2
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "rig", Path: rigPath}},
		Agents: []config.Agent{{
			Name:              "worker",
			Dir:               "rig",
			StartCommand:      "true",
			MaxActiveSessions: &maxSessions,
		}},
	}
	bp := newAgentBuildParams("test-city", cityPath, cfg, runtime.NewFake(), time.Now().UTC(), primary, io.Discard)
	rigStores := map[string]beads.Store{"rig": foreign}
	primeGuardedPoolCrossStoreCensus(t, bp, rigStores)
	if len(bp.sessionOccupancyInfos) != 0 {
		t.Fatalf("pre-lock census = %#v, want empty", bp.sessionOccupancyInfos)
	}

	_, qualifiedInstance, slot := poolDesiredRequestIdentity(&cfg.Agents[0], 1)
	expectedRuntimeName := poolRuntimeSessionName(cfg, qualifiedInstance, cfg.Agents[0].QualifiedName(), true)
	withLateForeignHolder := func(_ string, identifiers []string, fn func() error) error {
		if !containsString(identifiers, expectedRuntimeName) {
			t.Fatalf("identifier locks = %v, want exact runtime name %q", identifiers, expectedRuntimeName)
		}
		seedGuardedPoolSessionHolder(t, foreign, "late exact-name holder", "rig/manual", "", expectedRuntimeName)
		return fn()
	}
	created, err := createPoolSessionBeadWithGuardedAliasUsingLock(
		bp,
		&cfg.Agents[0],
		cfg.Agents[0].QualifiedName(),
		qualifiedInstance,
		slot,
		nil,
		withLateForeignHolder,
	)
	if !errors.Is(err, errPoolSessionNameUnavailable) {
		t.Fatalf("guarded create error = %v, want late foreign exact-name refusal", err)
	}
	if created.ID != "" || len(bp.sessionBeads.OpenInfos()) != 0 {
		t.Fatalf("refused create info=%#v writeback=%#v, want no primary mutation", created, bp.sessionBeads.OpenInfos())
	}
	primaryInfos, listErr := sessionFrontDoor(primary).ListAll(sessionpkg.ListAllOptions{})
	if listErr != nil || len(primaryInfos) != 0 {
		t.Fatalf("primary sessions = %#v err=%v, want none", primaryInfos, listErr)
	}
}

func TestCreatePoolSessionBeadWithGuardedAlias_LiveRecensusBypassesStaleForeignCache(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := t.TempDir()
	primaryBacking := beads.NewMemStore()
	primary := beads.NewCachingStoreForTest(primaryBacking, nil)
	// Both caches are fully primed (cacheLive) rather than active-only. A
	// partial prime answers no broad non-closed list query from cache at all,
	// so the ordinary census below would fall back to the backing store and
	// observe the external write immediately, collapsing the staleness window
	// this test exists to reproduce. A fully primed cache serves that census
	// from its own snapshot, which is the production shape: complete as of the
	// prime and still blind to a later write committed by another process.
	// Note the two unrelated senses of "live" here: this is the cache STATE,
	// whereas the lock-time recensus bypass below is session.ListAllOptions.Live.
	if err := primary.Prime(context.Background()); err != nil {
		t.Fatalf("prime primary cache: %v", err)
	}
	foreignBacking := beads.NewMemStore()
	foreign := beads.NewCachingStoreForTest(foreignBacking, nil)
	if err := foreign.Prime(context.Background()); err != nil {
		t.Fatalf("prime foreign cache: %v", err)
	}
	maxSessions := 2
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "rig", Path: rigPath}},
		Agents: []config.Agent{{
			Name:              "worker",
			Dir:               "rig",
			StartCommand:      "true",
			MaxActiveSessions: &maxSessions,
		}},
	}
	bp := newAgentBuildParams("test-city", cityPath, cfg, runtime.NewFake(), time.Now().UTC(), primary, io.Discard)
	rigStores := map[string]beads.Store{"rig": foreign}
	primeGuardedPoolCrossStoreCensus(t, bp, rigStores)

	_, qualifiedInstance, slot := poolDesiredRequestIdentity(&cfg.Agents[0], 1)
	expectedRuntimeName := poolRuntimeSessionName(cfg, qualifiedInstance, cfg.Agents[0].QualifiedName(), true)
	holder := seedGuardedPoolSessionHolder(t, foreignBacking, "external exact-name holder", "rig/manual", "", expectedRuntimeName)

	// The ordinary controller census deliberately stays cache-served. It misses
	// the external write until reconciliation, reproducing the production race
	// that made a non-Live lock-time read insufficient.
	stale, err := collectAllOpenSessionInfos(cityPath, cfg, primary, rigStores, nil)
	if err != nil {
		t.Fatalf("ordinary cached session census: %v", err)
	}
	for _, info := range stale {
		if info.ID == holder.ID {
			t.Fatalf("ordinary cached census unexpectedly observed external holder %#v", info)
		}
	}

	created, err := createPoolSessionBeadWithGuardedAlias(
		bp,
		&cfg.Agents[0],
		cfg.Agents[0].QualifiedName(),
		qualifiedInstance,
		slot,
		nil,
	)
	if !errors.Is(err, errPoolSessionNameUnavailable) {
		t.Fatalf("guarded create error = %v, want Live lock-time recensus to refuse cached foreign exact-name collision", err)
	}
	if created.ID != "" || len(bp.sessionBeads.OpenInfos()) != 0 {
		t.Fatalf("refused create info=%#v writeback=%#v, want no primary mutation", created, bp.sessionBeads.OpenInfos())
	}
	primaryInfos, listErr := sessionFrontDoor(primaryBacking).ListAll(sessionpkg.ListAllOptions{})
	if listErr != nil || len(primaryInfos) != 0 {
		t.Fatalf("primary backing sessions = %#v err=%v, want none", primaryInfos, listErr)
	}
}

func TestCreatePoolSessionBeadWithGuardedAlias_ForeignRecensusErrorNeverCreates(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := t.TempDir()
	primary := beads.NewMemStore()
	foreignBacking := beads.NewMemStore()
	foreign := &toggleListFailStore{Store: foreignBacking}
	maxSessions := 2
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "rig", Path: rigPath}},
		Agents: []config.Agent{{
			Name:              "worker",
			Dir:               "rig",
			StartCommand:      "true",
			MaxActiveSessions: &maxSessions,
		}},
	}
	bp := newAgentBuildParams("test-city", cityPath, cfg, runtime.NewFake(), time.Now().UTC(), primary, io.Discard)
	primeGuardedPoolCrossStoreCensus(t, bp, map[string]beads.Store{"rig": foreign})
	foreign.fail = true // the foreign leg degrades after planning, before lock-time proof

	_, qualifiedInstance, slot := poolDesiredRequestIdentity(&cfg.Agents[0], 1)
	created, err := createPoolSessionBeadWithGuardedAlias(
		bp,
		&cfg.Agents[0],
		cfg.Agents[0].QualifiedName(),
		qualifiedInstance,
		slot,
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "checking locked pool availability") || !strings.Contains(err.Error(), "session census leg \"rig:rig\"") || !strings.Contains(err.Error(), "cross-store list failed") {
		t.Fatalf("guarded create error = %v, want wrapped lock-time foreign recensus failure", err)
	}
	if created.ID != "" || len(bp.sessionBeads.OpenInfos()) != 0 {
		t.Fatalf("recensus-refused create info=%#v writeback=%#v, want no primary mutation", created, bp.sessionBeads.OpenInfos())
	}
	primaryInfos, listErr := sessionFrontDoor(primary).ListAll(sessionpkg.ListAllOptions{})
	if listErr != nil || len(primaryInfos) != 0 {
		t.Fatalf("primary sessions = %#v err=%v, want none", primaryInfos, listErr)
	}
}

func TestCreatePoolSessionBeadWithIdentifiers_UsesForeignAvailabilitySnapshotAndPrimaryWriteback(t *testing.T) {
	store := beads.NewMemStore()
	identity := poolSessionCreateIdentity{
		AgentName:     "worker-1",
		Slot:          1,
		TransientSlot: true,
	}
	identifiers, err := derivePoolSessionIdentifiers(nil, "worker", identity, "")
	if err != nil {
		t.Fatalf("derive pool identifiers: %v", err)
	}
	availabilitySnapshot := newSessionBeadSnapshotFromInfos([]sessionpkg.Info{{
		ID:                  "foreign-failed-create",
		SessionNameMetadata: identifiers.sessionName,
		MetadataState:       string(sessionpkg.StateFailedCreate),
	}})
	writebackSnapshot := newSessionBeadSnapshot(nil)

	info, err := createPoolSessionBeadWithIdentifiers(
		store,
		"worker",
		nil,
		availabilitySnapshot,
		writebackSnapshot,
		time.Now().UTC(),
		identity,
		identifiers,
	)
	if !errors.Is(err, errPoolSessionNameUnavailable) {
		t.Fatalf("create error = %v, want foreign OPEN failed-create holder to block exact name", err)
	}
	if info.ID != "" || len(writebackSnapshot.OpenInfos()) != 0 {
		t.Fatalf("blocked create info=%#v writeback=%#v, want neither result nor primary writeback", info, writebackSnapshot.OpenInfos())
	}

	// Once the foreign holder is absent from the complete availability census,
	// the same stable slot/name is reusable and the new primary row is written
	// back to the mutable primary snapshot.
	info, err = createPoolSessionBeadWithIdentifiers(
		store,
		"worker",
		nil,
		newSessionBeadSnapshot(nil),
		writebackSnapshot,
		time.Now().UTC(),
		identity,
		identifiers,
	)
	if err != nil {
		t.Fatalf("create after foreign holder closes: %v", err)
	}
	if info.SessionNameMetadata != identifiers.sessionName {
		t.Fatalf("replacement session_name = %q, want stable %q", info.SessionNameMetadata, identifiers.sessionName)
	}
	if got := writebackSnapshot.OpenInfos(); len(got) != 1 || got[0].ID != info.ID {
		t.Fatalf("primary writeback = %#v, want newly created session %s", got, info.ID)
	}
}
