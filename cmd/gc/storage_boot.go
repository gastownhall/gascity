package main

// Storage-class routing at boot: registry → effective config → plan → verdict
// → routes.
//
// This is the one place a controller process decides where each coordination
// class is served from, and it decides it once. The registry is built and
// frozen here, the plan is resolved here, the convergence verdict is taken
// here, and the opened engine is held here for the life of the process. Live
// storage handles are immutable, so a config reload that changes [storage]
// refuses the whole reload rather than re-entering this file — the check is
// config.StorageReloadRequiresRestart, wired in CityRuntime.reloadConfigTraced.
//
// # No config is a bypass, not an equivalent path
//
// A city with no [storage] section short-circuits at the top of the gate,
// before a registry is constructed, before a plan is resolved, and before a
// single byte is read from a binding root. Routes are nil, every class resolver
// stays the identity it is today, and the compatibility claim is therefore true
// by construction rather than by an equivalence argument over two code paths
// that happen to agree. TestStorageGateBypassesEverythingWithoutConfig pins it.
//
// The bypass asks exactly one question first, and the question is not about
// configuration: has this city ever served its infrastructure classes from a
// binding? That is one read of one path in the city's own .gc, absent on every
// city the compatibility contract was written for, and it costs them a failed
// open and nothing else — no registry, no plan, no binding root. It is here
// because without it the deletion of the [storage] section was the one edit
// that walked past every hold in this file, and the refusals were arranged so
// that an operator trying to undo a split would find it.
//
// Always resolving the default plan was the alternative and was rejected: plan
// resolution takes pinned Work inputs whose drift checks are a refusal mode,
// and a city that never authored [storage] must not gain one.
//
// # Boot refuses; it never migrates
//
// The verdict this gate takes is three-valued in effect: serve, bypass, or
// refuse. There is no fourth arm that fixes anything. A city whose config names
// a binding it has not converged on is a city whose infrastructure state is
// somewhere other than where its readers are about to look, and the only safe
// thing a booting binary can do about that is stop and say so — with the name
// of the command that performs the move. See infra_class_migrate.go for why the
// copy cannot be on this path.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/storebinding"
	"github.com/gastownhall/gascity/internal/storeref"
)

// storageSupportedTopologyStatement is the one sentence describing what this
// build can serve. It is a constant because the boot refusal, the migrate
// command and the documentation must all describe the same runtime.
const storageSupportedTopologyStatement = "This build serves the whole infrastructure split or none of it: either every class stays on the reserved \"work\" binding, or work stays there and graph/sessions/messaging/orders/nudges all move to one shared binding."

// storageRoutes is the opened non-work binding of a resolved plan, keyed by the
// class it serves.
//
// A nil *storageRoutes is the identity routing every city has today, which is
// why the accessor is a method on the pointer and tolerates nil: the no-config
// path passes nil and every class resolver takes the same branch it takes now.
type storageRoutes struct {
	// stores maps a coordination class to the store that serves it. Only
	// classes assigned to a non-work binding appear; a class routed to the
	// reserved work binding is absent, so it falls through to the work store
	// the caller already holds.
	stores map[coordclass.Class]beads.Store
	// closers owns the durable resources the opened bindings hold, in open
	// order. The process closes them once, on shutdown.
	closers []io.Closer
	// binding names the non-work binding these routes were opened from, for
	// diagnostics.
	binding string
	// emitCityPath is the city whose event log these routes' class stores
	// append bead.* events to, and it is set on exactly one construction:
	// the one-shot CLI funnel's (cli_storage_routes.go). openStorageRoutes
	// leaves it empty, so the controller's routes carry no emit target and
	// its CachingStore stays the only emitter on that side — see
	// class_store_emit.go for why the two must not both emit.
	emitCityPath string
	// relics records, per binding store, whether the boot-time census found a
	// bead — open or closed — outside the namespaces that binding declares.
	//
	// ABSENT MEANS UNKNOWN, and unknown means "assume relics" — see
	// hasLegacyResidents. A process that never censused, or a binding whose
	// mint bit is off so the answer would not matter, must not read as clean.
	//
	// Keying a map on beads.Store is safe here for the same confined reason
	// residencyBindingsFromRoutes gives: every key comes from this struct's own
	// stores map, which holds what storage boot opened.
	relics map[beads.Store]bool
}

// hasLegacyResidents reports the boot census's verdict for a binding store.
//
// The default is the pessimistic one on every path that could skip the census:
// a nil receiver, an uncensused process, and a store the census never reached
// all answer true. Retirement is the claim that needs evidence.
func (r *storageRoutes) hasLegacyResidents(store beads.Store) bool {
	if r == nil {
		return true
	}
	verdict, censused := r.relics[store]
	if !censused {
		return true
	}
	return verdict
}

// censusBindingRelics takes the once-per-process live read that turns
// ClassBinding.HasLegacyResidents from a pessimistic default into an
// observation.
//
// It runs only for a binding whose mint bit already verified, which is both a
// cost bound and a statement of what the answer is for: probe retirement needs
// BOTH halves, so a binding that does not mint truthfully pays nothing to learn
// an answer that cannot change its plans.
//
// Since ga-qdt5y.18 this verdict is load-bearing on `gc bd`'s by-id path, which
// is the busiest one-shot route in the CLI: the door no longer keeps a probe of
// its own, so a binding certified clean here is a binding that path will not
// read. A false clean is a lost bead, which is why every unknown answers true.
//
// # The cost, and why it is paid rather than written down
//
// The scan lists the binding with AllowScan and IncludeClosed, so it costs the
// binding's WHOLE history and not its working set: 7.2ms at 1k rows, 82ms at
// 10k, linear at ~8.2us/row (internal/storeref/relic_census_bench_test.go).
// "Once per process" is what bounds it — the one-shot funnel takes it inside a
// sync.Once per city (cliStorageRoutes) and the controller takes it once at boot
// — and the verdict lives in the routes value those callers already hold.
//
// It is deliberately NOT remembered on disk. A note saying "this binding holds
// relics" is a status file: it survives an operator rebuilding or re-pointing
// the binding, nothing clears it, and a later `gc` then keeps or retires a probe
// on a verdict no store agrees with. Querying live state is the house rule
// (AGENTS.md) and the residual is a cheaper scan, not a cache — bounding it
// needs a store-level id-namespace predicate beads.ListQuery does not have
// (ga-dx4ho). TestBootCensusIsLiveAndLeavesNothingOnDisk pins both halves.
func censusBindingRelics(routes *storageRoutes) {
	if routes == nil {
		return
	}
	// Built with the pessimistic default still in force — the census reads the
	// bindings' stores and prefixes, never their relic bits — so this is not
	// circular. A refusal here is carried by the bindings themselves; the
	// census's own error handling is what makes a refused binding keep its
	// probe.
	bindings, _ := residencyBindingsFromRoutes(routes)
	relics := make(map[beads.Store]bool, len(bindings))
	for _, binding := range bindings {
		if !binding.MintsReserved {
			continue
		}
		relics[binding.Leg.Store] = storeref.HasLegacyResidents(binding) // residency:allow — indexes a census result by the binding it was taken from; resolves nothing
	}
	routes.relics = relics
}

// storeFor returns the store serving a class and whether these routes relocate
// it at all. A nil receiver relocates nothing.
func (r *storageRoutes) storeFor(class coordclass.Class) (beads.Store, bool) {
	if r == nil {
		return nil, false
	}
	store, ok := r.stores[class]
	return store, ok
}

// close releases every engine these routes opened, in reverse open order.
func (r *storageRoutes) close() error {
	if r == nil {
		return nil
	}
	var errs []error
	for i := len(r.closers) - 1; i >= 0; i-- {
		if err := r.closers[i].Close(); err != nil {
			errs = append(errs, err)
		}
	}
	r.closers = nil
	return errors.Join(errs...)
}

// storageSplitShape is the class-to-binding arrangement this build recognizes.
type storageSplitShape int

const (
	// storageSplitNone is every class on the reserved work binding. It routes
	// nothing and is exactly today's behavior.
	storageSplitNone storageSplitShape = iota
	// storageSplitWhole is work on the reserved binding and all five
	// infrastructure classes on one shared non-work binding.
	storageSplitWhole
	// storageSplitUnsupported is any other arrangement: a partial move, a
	// per-class fan-out, or work relocated off the reserved binding. The plan
	// machinery can resolve these; this runtime cannot serve them, and routing
	// part of a split by silence is the failure this gate exists to prevent.
	storageSplitUnsupported
)

// storageSplitShapeOf classifies a city's effective class assignment.
//
// It reads only configuration and touches no filesystem, so it is the same
// answer on a city that has migrated and one that has not — which is the point:
// the shape decides whether this runtime CAN serve the city, and convergence
// decides whether it MAY.
func storageSplitShapeOf(storage config.StorageConfig) (storageSplitShape, string) {
	if storage.Classes.Work != config.StorageWorkBinding {
		return storageSplitUnsupported, ""
	}
	shared := storage.Classes.BindingFor(infraMigrationClasses[0])
	for _, class := range infraMigrationClasses[1:] {
		if storage.Classes.BindingFor(class) != shared {
			return storageSplitUnsupported, ""
		}
	}
	if shared == config.StorageWorkBinding {
		return storageSplitNone, ""
	}
	if shared == "" {
		return storageSplitUnsupported, ""
	}
	return storageSplitWhole, shared
}

// storageBootGate decides how this process serves each coordination class, and
// refuses to start the city when config and reality disagree.
//
// A nil routes value with a nil error is the identity path: the city leaves
// every class on the reserved work binding — by authoring no [storage] at all,
// or by authoring one that says so — and has never served them anywhere else.
// A non-nil error is a refusal, already carrying the operator instruction; the
// caller prints it and stops.
func storageBootGate(cityPath string, cfg *config.City, logPrefix string, rec events.Recorder, stderr io.Writer) (*storageRoutes, error) {
	// The bypass, first — and now with the one question a city that HAS served
	// a split can answer differently from a city that never did.
	//
	// Deleting the whole [storage] section used to be the way past every hold
	// in this file: pointing the classes back at work is refused while the
	// served-binding note stands, but deleting the section that names them
	// reached this line and served everything from the work store while the
	// city's real infrastructure state sat in a binding nothing would read
	// again. The refusal ladder was steering operators into exactly that edit.
	//
	// The compatibility contract survives intact, because the question costs
	// one failed read of one path. A city that never configured storage has no
	// note, takes this branch, and reaches no registry, no plan and no binding
	// root — which is the property the contract was written to protect, and is
	// still pinned as a negative.
	//
	// The shape passed to the hold is the one an absent section resolves to —
	// every class on the work binding — because that is what deleting it
	// means, and it is the same question the explicit spelling asks below.
	if cfg == nil || cfg.Storage == nil {
		if blocked, held := revertHoldingNote(storageSplitNone, cityPath); held {
			advice := infraMigrationOperatorAdvice(blocked, logPrefix)
			recordStorageBindingOutcome(rec, blocked, advice)
			return nil, errors.New(advice)
		}
		recordStorageBindingOutcome(rec, infraMigrationReport{Outcome: infraMigrationNotConfigured}, "")
		return nil, nil
	}

	storage := cfg.EffectiveStorage()
	shape, binding := storageSplitShapeOf(storage)
	if shape == storageSplitUnsupported {
		return nil, fmt.Errorf("%s: [storage.classes] describe an arrangement this build cannot serve. %s", logPrefix, storageSupportedTopologyStatement)
	}

	// Resolved once, for both shapes: an unknown provider, a defined reserved
	// binding, an unreferenced binding or an unusable Work pin is a refusal
	// here rather than a surprise at the first read.
	plan, err := resolveCityStoragePlan(cityPath, cfg)
	if err != nil {
		// A half-finished revert reaches here — classes pointed back at work
		// while the binding they left is still defined, which plan resolution
		// correctly rejects as unreferenced. That message describes the config
		// and hides the load-bearing one, so the note's refusal is joined onto
		// it rather than left for the operator to discover after the edit that
		// makes this error go away. Plan resolution stays first: refusing
		// everything it can before any binding is touched is its whole point.
		if blocked, held := revertHoldingNote(shape, cityPath); held {
			return nil, errors.Join(fmt.Errorf("%s: %w", logPrefix, err), errors.New(infraMigrationOperatorAdvice(blocked, logPrefix)))
		}
		return nil, fmt.Errorf("%s: %w", logPrefix, err)
	}
	if shape == storageSplitNone {
		// The natural revert edit — every class pointed back at work — is
		// exactly the re-point the served-binding note exists to hold. A city
		// that never served a split has no note and passes untouched.
		if blocked, held := revertHoldingNote(shape, cityPath); held {
			advice := infraMigrationOperatorAdvice(blocked, logPrefix)
			recordStorageBindingOutcome(rec, blocked, advice)
			return nil, errors.New(advice)
		}
		// Same verdict as the absent section above, and deliberately the same
		// event: an operator who reverted by re-pointing every class and one who
		// deleted the section have arrived at the same place, and a subscriber
		// should not have to know which edit they made to read it.
		recordStorageBindingOutcome(rec, infraMigrationReport{Outcome: infraMigrationNotConfigured}, "")
		return nil, nil
	}

	target, ok, err := resolveInfraBindingTarget(cityPath, cfg)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", logPrefix, err)
	}
	var report infraMigrationReport
	if ok {
		report = checkInfraClassConvergence(cityPath, cfg, logPrefix, stderr)
	} else {
		// The shape is the supported one, so the only thing that can have
		// refused the target is the provider: this build carries the migration
		// and genesis discipline for its own bead engine and for nothing else.
		// A compiled provider that opens a bead engine can still serve — under
		// the born-split discipline below, which asks nothing of the binding
		// and proves the one invariant the work store alone can prove.
		target = infraBindingTarget{Binding: binding}
		// A provider that opens no bead engine cannot serve regardless of
		// what the work store holds. Refusing here, before any outcome is
		// recorded, keeps the event stream honest: a permanently unservable
		// binding must not publish converged on every boot.
		if opener := plannedBindingOpener(plan, binding); opener == nil {
			return nil, fmt.Errorf("%s: binding %q is served by provider %q, which does not open a bead engine, so the classes assigned to it cannot be served; %s",
				logPrefix, binding, storage.Bindings[binding].Provider, contract.BackendNotOpenedGuarantee)
		}
		location, err := servedBindingLocation(plan, binding, storage.Bindings[binding])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", logPrefix, err)
		}
		if blocked, held := servedBindingNoteHold(cityPath, binding, storage.Bindings[binding].Provider, location); held {
			report = blocked
			report.Target = target
		} else {
			report = checkBornSplitDiscipline(cityPath, logPrefix, stderr)
			report.Target = target
		}
	}
	if !report.serving() {
		advice := infraMigrationOperatorAdvice(report, logPrefix)
		if advice == "" {
			advice = fmt.Sprintf("%s: binding %q cannot be served and the reason was not reported (%s)", logPrefix, target.Binding, report.Outcome)
		}
		recordStorageBindingOutcome(rec, report, advice)
		return nil, errors.New(advice)
	}
	// The serving outcome is published before the binding opens, so a boot
	// whose open then fails has already reported converged. That ordering
	// predates this file's note handling and is left as it is: the event
	// stream reports the verdict this gate reached, and the open's own failure
	// is what the caller prints.
	recordStorageBindingOutcome(rec, report, "")
	routes, err := openStorageRoutes(plan, target)
	if err != nil {
		return nil, err
	}
	if err := recordServedBinding(plan, cityPath, storage, target); err != nil {
		return nil, errors.Join(fmt.Errorf("%s: %w", logPrefix, err), routes.close())
	}
	// One read, here, because this is the single funnel both planes pass
	// through — the controller's boot and the one-shot CLI's memoized routes.
	// Taking it anywhere downstream would put a store read inside a per-tick
	// topology constructor.
	censusBindingRelics(routes)
	return routes, nil
}

// revertHoldingNote reports whether a city pointing its classes back at the
// work binding is held by a note saying it has served them somewhere else.
//
// One spelling, because two paths ask it: the shape that says every class is
// on work, and the plan failure that a half-finished revert produces before
// the shape is ever acted on.
func revertHoldingNote(shape storageSplitShape, cityPath string) (infraMigrationReport, bool) {
	if shape != storageSplitNone {
		return infraMigrationReport{}, false
	}
	blocked, held := servedBindingNoteHold(cityPath, config.StorageWorkBinding, "", "")
	if !held {
		return infraMigrationReport{}, false
	}
	blocked.Target = infraBindingTarget{Binding: config.StorageWorkBinding}
	return blocked, true
}

// recordServedBinding writes the durable record of which binding served this
// city's infrastructure classes, and where.
//
// It runs AFTER the binding opened, and the order is the whole point. The note
// is history: every later boot or migration that points the classes somewhere
// else refuses until an operator attests by deleting it. Written before the
// open, a city that never served — a mistyped configuration reference, a
// workspace that is not there — acquired that history anyway, and then fixing
// the typo was itself a re-point the note refused. Nothing is lost by waiting:
// the note exists so a later genesis cannot bless an empty store while the
// city's real state sits in a binding, and a binding that never opened holds
// no state to orphan.
func recordServedBinding(plan *storebinding.StoragePlan, cityPath string, storage config.StorageConfig, target infraBindingTarget) error {
	bindingCfg := storage.Bindings[target.Binding]
	location, err := servedBindingLocation(plan, target.Binding, bindingCfg)
	if err != nil {
		return err
	}
	note := bornSplitServedNote{Binding: target.Binding, Provider: bindingCfg.Provider, Location: location}
	if err := writeServedBindingNote(cityPath, note); err != nil {
		return fmt.Errorf("recording the served-binding note, the durable record of which binding serves this city's infrastructure classes: %w", err)
	}
	return nil
}

// writeServedBindingNote is the durable write of the served-binding note.
//
// It is a variable so a test can fail it AFTER a binding has opened. That is
// the one ordering the move to post-open created and nothing else can reach:
// the routes exist, the note does not, and whether the boot releases the
// engine it just opened is invisible from the returned error alone.
var writeServedBindingNote = writeBornSplitServedNote

// servedBindingLocation reports WHERE a binding serves from, in the one
// spelling every reader and writer of the served-binding note must agree on.
//
// The provider answers when it can, because only the provider knows how its
// own configuration becomes a location — and because a configuration reference
// is not an answer: two cities can carry the identical reference and serve
// from different directories, and a note holding the reference cannot tell
// them apart. A provider that offers no locator falls back to what it was
// configured with, which is what a city recorded before this seam existed.
//
// A locator that fails is a refusal, not a fallback: a provider that cannot
// say where it serves must not have a note written about it.
//
// One spelling remains outside this function: the built-in arm's own hold
// check in inspectInfraConvergence passes the migration's resolved database
// directly. The two agree by construction and are kept agreeing by
// TestServedBindingLocationIsWhereTheProviderOpens; they are not merged here
// because that call sites holds a resolved target and no plan.
func servedBindingLocation(plan *storebinding.StoragePlan, name string, binding config.StorageBindingConfig) (string, error) {
	if plan != nil {
		for _, planned := range plan.Bindings() {
			if string(planned.Name) != name {
				continue
			}
			locator, ok := storebinding.BindingLocatorFor(planned)
			if !ok {
				break
			}
			location, err := locator.BindingLocation(planned.Spec)
			if err != nil {
				return "", fmt.Errorf("resolving where binding %q serves from: %w", name, err)
			}
			return location, nil
		}
	}
	return configuredBindingLocation(binding), nil
}

// bornSplitServedNote records the durable fact that this city has served its
// infrastructure classes from a binding this build cannot read. It is not
// process state — it is history, the same durability class as the convergence
// marker, and the one input that keeps a later genesis from blessing an empty
// store while the city's real state lives in that binding.
type bornSplitServedNote struct {
	Binding  string `json:"binding"`
	Provider string `json:"provider"`
	// Location pins WHERE the binding serves from — the resolved database
	// path for this build's engine, the configured reference otherwise — so
	// a re-point that keeps the binding's name but moves its storage cannot
	// slip past a name-and-provider comparison.
	Location string `json:"location"`
}

// configuredBindingLocation is the location a note records for a binding this
// build cannot resolve: the opaque reference its provider does, or the path
// when a spec carries one.
func configuredBindingLocation(binding config.StorageBindingConfig) string {
	if binding.ConfigRef != "" {
		return binding.ConfigRef
	}
	return binding.Path
}

// bornSplitServedNotePath is where the note lives, under the city's own .gc.
func bornSplitServedNotePath(cityPath string) string {
	return filepath.Join(cityPath, ".gc", "storage-served-binding.json")
}

// readBornSplitServedNote reads the note if one exists. An unreadable or
// undecodable note is an error, never an absence: absence is the license
// genesis acts on, and a corrupt file must not grant it.
func readBornSplitServedNote(cityPath string) (bornSplitServedNote, bool, error) {
	data, err := os.ReadFile(bornSplitServedNotePath(cityPath))
	if errors.Is(err, fs.ErrNotExist) {
		return bornSplitServedNote{}, false, nil
	}
	if err != nil {
		return bornSplitServedNote{}, false, err
	}
	var note bornSplitServedNote
	if err := json.Unmarshal(data, &note); err != nil {
		return bornSplitServedNote{}, false, fmt.Errorf("decoding %s: %w", bornSplitServedNotePath(cityPath), err)
	}
	return note, true, nil
}

// writeBornSplitServedNote durably records the served binding before the first
// serve. Atomic replace, idempotent, and a failure refuses the boot: serving
// without the note is what re-opens the genesis hole this note closes.
func writeBornSplitServedNote(cityPath string, note bornSplitServedNote) error {
	path := bornSplitServedNotePath(cityPath)
	if existing, present, err := readBornSplitServedNote(cityPath); err != nil {
		return err
	} else if present && existing == note {
		return nil
	}
	data, err := json.Marshal(note)
	if err != nil {
		return fmt.Errorf("encoding the served-binding note: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".storage-served-binding-*")
	if err != nil {
		return fmt.Errorf("staging the served-binding note: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		_ = os.Remove(temp.Name())
		return fmt.Errorf("writing the served-binding note: %w", err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(temp.Name())
		return fmt.Errorf("closing the served-binding note: %w", err)
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		_ = os.Remove(temp.Name())
		return fmt.Errorf("installing the served-binding note: %w", err)
	}
	return nil
}

// servedBindingNoteHold is the one rule every serving and migrating path asks:
// does the served-binding note name a different binding or provider than the
// one this path is about to serve or migrate onto? A mismatch is a hold — the
// note's binding holds infrastructure state the new target cannot read — and
// an unreadable note holds exactly as a readable one would, because a corrupt
// file must not grant what its absence grants. The note's removal is the
// operator's attestation, and the refusal string carries everything.
func servedBindingNoteHold(cityPath, binding, provider, location string) (infraMigrationReport, bool) {
	note, present, err := readBornSplitServedNote(cityPath)
	if err != nil {
		return infraMigrationReport{
			Outcome:        infraMigrationGenesisBlocked,
			ServedNotePath: bornSplitServedNotePath(cityPath),
		}, true
	}
	if !present || (note.Binding == binding && note.Provider == provider && note.Location == location) {
		return infraMigrationReport{}, false
	}
	return infraMigrationReport{
		Outcome:        infraMigrationGenesisBlocked,
		ServedBinding:  note.Binding,
		ServedProvider: note.Provider,
		ServedNotePath: bornSplitServedNotePath(cityPath),
	}, true
}

// checkBornSplitDiscipline decides whether a binding served by a provider this
// build carries no migration discipline for may serve this city's split.
//
// Such a provider can still open the binding's bead engine — that is the
// EngineOpener seam — but nothing in this build can move a bead onto its
// binding, prove a marker about it, or read a manifest from it: the whole
// convergence apparatus is written against this build's own engine. What CAN
// be proven, from the work store alone, is the one invariant that matters for
// a city born on the split: no infrastructure bead has ever landed in the
// work store. So the discipline is containment with nothing persisted,
// re-proven on every boot — serve while the work store holds zero
// infrastructure beads, and refuse naming ids the moment one exists, whether
// it was written by an earlier configuration, by a writer that never saw this
// [storage] section, or by a city that in fact predates the split and needs a
// migration this build cannot perform.
//
// Failures to prove are reported as facts about the check, not the city: a
// work store that cannot be opened or listed decides nothing, and the
// uncheckable outcome withholds both serving and the revert instruction.
func checkBornSplitDiscipline(cityPath string, logPrefix string, stderr io.Writer) infraMigrationReport {
	source, err := openInfraMigrationSource(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: born-split check: opening the work store: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return infraMigrationReport{Outcome: infraMigrationUncheckable, Fault: fmt.Errorf("born-split check: opening the work store: %w", err)}
	}
	defer closeBeadStoreHandle(source) //nolint:errcheck // best-effort close
	infra, err := readInfraSnapshot(source)
	if err != nil {
		fmt.Fprintf(stderr, "%s: born-split check: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return infraMigrationReport{Outcome: infraMigrationUncheckable, Fault: fmt.Errorf("born-split check: %w", err)}
	}
	if len(infra) == 0 {
		return infraMigrationReport{Outcome: infraMigrationConverged}
	}
	ids := make([]string, 0, len(infra))
	for _, b := range infra {
		ids = append(ids, b.ID)
	}
	sort.Strings(ids)
	return infraMigrationReport{Outcome: infraMigrationBornSplitBlocked, Stranded: ids}
}

// plannedBindingOpener returns the engine-opening hook for the named binding
// in a resolved plan, or nil when the plan does not carry the binding or its
// provider opens no engine.
func plannedBindingOpener(plan *storebinding.StoragePlan, name string) storebinding.EngineOpener {
	if plan == nil {
		return nil
	}
	for _, planned := range plan.Bindings() {
		if string(planned.Name) != name {
			continue
		}
		if opener, ok := storebinding.EngineOpenerFor(planned); ok {
			return opener
		}
		return nil
	}
	return nil
}

// storageBindingEventTypes maps a migration outcome to the event that reports
// it. Every outcome has one, and TestEveryMigrationOutcomeReachesARegisteredEventType
// keeps it that way: an unmapped outcome publishes nothing and does so silently,
// so a subscriber gating a deploy would read a verdict that was reached as no
// verdict at all.
//
// Several refusals share the unconverged type on purpose — a stranded city, a
// blocked born split and a blocked genesis are all "config and data disagree",
// and the sentence in the payload's invariant is what distinguishes them. The
// distinct types are the ones a subscriber would branch on rather than read.
var storageBindingEventTypes = map[infraMigrationOutcome]string{
	infraMigrationNotConfigured:    events.StorageBindingNotConfigured,
	infraMigrationConverged:        events.StorageBindingConverged,
	infraMigrationGenesis:          events.StorageBindingGenesis,
	infraMigrationUnconverged:      events.StorageBindingUnconverged,
	infraMigrationStranded:         events.StorageBindingUnconverged,
	infraMigrationBornSplitBlocked: events.StorageBindingUnconverged,
	infraMigrationGenesisBlocked:   events.StorageBindingUnconverged,
	infraMigrationUncheckable:      events.StorageBindingUncheckable,
}

// recordStorageBindingOutcome publishes what one gate or one migration
// concluded about a binding.
//
// invariant is the operator-facing sentence a refusal printed, so a subscriber
// reads the same reason the terminal did rather than a paraphrase of it.
func recordStorageBindingOutcome(rec events.Recorder, report infraMigrationReport, invariant string) {
	if rec == nil {
		return
	}
	eventType, ok := storageBindingEventTypes[report.Outcome]
	if !ok {
		return
	}
	raw, err := json.Marshal(storebinding.StorageBindingOutcomePayload{
		Binding:     report.Target.Binding,
		Database:    report.Target.Database,
		Outcome:     report.Outcome.String(),
		Invariant:   invariant,
		ProvenBeads: report.ProvenBeads,
	})
	if err != nil {
		return
	}
	rec.Record(events.Event{
		Type:    eventType,
		Actor:   "gc",
		Subject: report.Target.Binding,
		Payload: raw,
	})
}

// openStorageRoutes opens the planned non-work binding and keys its store by
// every class the plan assigns to it.
//
// The store comes from the provider's own EngineOpener rather than from a
// database this file opens by hand. That is the whole extension point: a
// downstream fork registers its factory and implements the same seam, and
// nothing here changes. A planned binding whose provider does not implement it
// is a refusal that names the provider — never a fall-through to the work
// store, which would serve a relocated class out of the ledger it was moved off.
func openStorageRoutes(plan *storebinding.StoragePlan, target infraBindingTarget) (*storageRoutes, error) {
	if plan == nil {
		return nil, errors.New("storage routing: no resolved plan")
	}
	var planned storebinding.PlannedBinding
	found := false
	for _, binding := range plan.Bindings() {
		if string(binding.Name) == target.Binding {
			planned = binding
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("storage routing: the resolved plan carries no binding %q", target.Binding)
	}
	opener, ok := storebinding.EngineOpenerFor(planned)
	if !ok {
		return nil, fmt.Errorf("storage routing: binding %q is served by provider %q, which does not open a bead engine, so the classes assigned to it cannot be served; %s",
			target.Binding, planned.ProviderID, contract.BackendNotOpenedGuarantee)
	}
	store, closer, err := opener.OpenEngine(planned.Spec, planned.AssignedClasses)
	if err != nil {
		return nil, fmt.Errorf("storage routing: opening binding %q: %w", target.Binding, err)
	}
	routes := &storageRoutes{
		stores:  make(map[coordclass.Class]beads.Store, len(planned.AssignedClasses.Classes())),
		closers: []io.Closer{closer},
		binding: target.Binding,
	}
	for _, class := range planned.AssignedClasses.Classes() {
		routes.stores[class] = store
	}
	return routes, nil
}

// newStorageRegistryForPlan is the compiled registry this file resolves plans
// against. It is a variable so a test can COUNT constructions and prove the
// no-config path performs none — the compatibility claim is a negative, and a
// negative needs an observer.
var newStorageRegistryForPlan = newStorageProviderRegistry

// resolveCityStoragePlan resolves this city's storage plan exactly once against
// the frozen compiled registry.
func resolveCityStoragePlan(cityPath string, cfg *config.City) (*storebinding.StoragePlan, error) {
	registry, err := newStorageRegistryForPlan()
	if err != nil {
		return nil, err
	}
	// The city root is stamped into every binding specification here, once.
	// It is what a provider resolves its city-relative configuration against,
	// and it must be this city's path rather than the process's directory: one
	// supervisor process hosts every registered city, so the two are the same
	// only for a command run inside the city it is acting on.
	//
	// An empty city path is refused rather than absolutized, because
	// filepath.Abs("") is the working directory — the exact base this stamp
	// exists to stop a provider from using, reintroduced through the back door
	// by a caller that simply had no city to name.
	if strings.TrimSpace(cityPath) == "" {
		return nil, errors.New("resolving the storage plan: no city path, and a binding's location is resolved against the city that declares it")
	}
	root, err := filepath.Abs(cityPath)
	if err != nil {
		return nil, fmt.Errorf("resolving the city root %q: %w", cityPath, err)
	}
	plan, err := storebinding.ResolveStoragePlan(registry, cfg.EffectiveStorage(), cityStorageWorkPins(cityPath, cfg), root)
	if err != nil {
		return nil, fmt.Errorf("resolving the storage plan: %w", err)
	}
	return plan, nil
}

// cityStorageWorkPins describes this city's work scopes to the plan resolver.
//
// The plan pins the Work topology so a later phase cannot re-point a class at
// whatever mutable workspace metadata happens to say. What this build can pin
// is what its own configuration states: one HQ scope plus one scope per
// configured rig, each with the bead prefix that scope mints under and a
// physical identity derived from the store root it resolves to.
//
// Two scopes resolving the same root therefore carry the SAME physical
// identity, which is the durable statement that they are one workspace on disk
// and is what the drift check compares per scope. It is not a grouping:
// groupPinnedPhysical keys on the whole (opener, component, physical) triple,
// and each rig pins its own component, so two rigs sharing a root stay two
// pinned scopes and two groups. Nothing here declares otherwise.
//
// It reads no workspace metadata and performs no I/O, which is why Recorded is
// false and Observed is empty: this is a description, not a durable record, and
// a description must not be able to trip the drift refusal that exists to
// protect recorded pins.
func cityStorageWorkPins(cityPath string, cfg *config.City) storebinding.WorkPinInputs {
	// The pin has to state the prefix HQ actually mints under, which is the
	// full three-step resolution and not the resolved field alone: no default
	// city.toml declares [workspace] prefix, so an ordinary city derives its
	// prefix from its name. Reading the field alone sent every such city to
	// the fallback, and a fallback that repeats a rig's prefix collides with
	// it. The rig arm below already resolves through Rig.EffectivePrefix.
	//
	// The fallback now stands only where the city resolves no prefix at all,
	// rather than standing in for every city that leaves the field unset.
	pins := storebinding.WorkPinInputs{
		ConfigContext: storageWorkConfigContext(cityPath, cfg),
		HQ: storebinding.WorkScopePin{
			Scope:       storebinding.HQScope(),
			Prefix:      storageWorkPrefix(config.EffectiveHQPrefix(cfg), "gc"),
			OpenerID:    storageWorkOpenerID(cfg),
			ComponentID: "hq",
			PhysicalID:  storageWorkPhysicalID(cityPath),
		},
	}
	if cfg == nil {
		return pins
	}
	for _, rig := range cfg.Rigs {
		root := strings.TrimSpace(rig.Path)
		if root == "" {
			// An unbound rig has no workspace of its own to pin. Pinning one
			// would invent a second scope on the city's own root, which the
			// resolver would correctly read as an aliased prefix.
			continue
		}
		pins.Rigs = append(pins.Rigs, storebinding.WorkScopePin{
			Scope:       storebinding.RigScope(rig.Name),
			Prefix:      storageWorkPrefix(rig.EffectivePrefix(), "gr"),
			Suspended:   rig.Suspended,
			OpenerID:    storageWorkOpenerID(cfg),
			ComponentID: storageWorkComponentID(rig.Name),
			PhysicalID:  storageWorkPhysicalID(root),
		})
	}
	return pins
}

// storageWorkConfigContext digests the configuration the pins were derived
// from, so a plan carries a canonical reference to the inputs that produced it.
//
// It digests the same resolved values the pins carry, not the raw fields they
// were resolved from. Hashing a field the pin no longer reads would let two
// cities whose HQ pins genuinely differ share one digest, and the digest exists
// precisely to tell those two apart. The rig line already digests the resolved
// prefix for the same reason.
func storageWorkConfigContext(cityPath string, cfg *config.City) storebinding.ConfigRefDigest {
	sum := sha256.New()
	fmt.Fprintln(sum, cityPath) //nolint:errcheck // hashing a writer that cannot fail
	if cfg != nil {
		fmt.Fprintln(sum, config.EffectiveHQPrefix(cfg)) //nolint:errcheck // hashing a writer that cannot fail
		for _, rig := range cfg.Rigs {
			fmt.Fprintf(sum, "%s\x00%s\x00%s\x00%t\n", rig.Name, rig.EffectivePrefix(), rig.Path, rig.Suspended) //nolint:errcheck // hashing a writer that cannot fail
		}
	}
	return storebinding.ConfigRefDigest("sha256:" + hex.EncodeToString(sum.Sum(nil)))
}

// storageWorkOpenerID names the mechanism that opens this city's work scopes.
// It is a provider name rather than a path, so two scopes on the same root and
// the same opener group together and two on different openers do not.
func storageWorkOpenerID(cfg *config.City) string {
	if cfg != nil {
		if provider := storageWorkIdentifier(cfg.Beads.Provider); provider != "" {
			return provider
		}
	}
	return "default"
}

// storageWorkComponentID renders a rig name as a pin component identifier.
func storageWorkComponentID(name string) string {
	if id := storageWorkIdentifier(name); id != "" {
		return id
	}
	return "rig"
}

// storageWorkPhysicalID identifies one physical work workspace by the root it
// resolves to. It is a digest rather than the path because a pin identifier is
// a restricted-character token and a filesystem path is not.
func storageWorkPhysicalID(root string) string {
	clean := root
	if abs, err := filepath.Abs(root); err == nil {
		clean = abs
	}
	sum := sha256.Sum256([]byte(filepath.Clean(clean)))
	return hex.EncodeToString(sum[:])
}

// storageWorkPrefix returns a usable pinned bead prefix, falling back when the
// configuration does not state one.
func storageWorkPrefix(prefix, fallback string) string {
	trimmed := strings.ToLower(strings.TrimSpace(prefix))
	trimmed = strings.Trim(trimmed, "-")
	if trimmed == "" {
		return fallback
	}
	return trimmed
}

// storageWorkIdentifier reduces a configured name to the character set a pin
// identifier admits, or "" when nothing usable survives.
func storageWorkIdentifier(value string) string {
	var out strings.Builder
	for _, r := range strings.TrimSpace(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			out.WriteRune(r)
		default:
			out.WriteByte('-')
		}
	}
	return strings.Trim(out.String(), "-")
}

// storageClassOrder returns the coordination classes in a stable reporting
// order: work first, then the infrastructure classes in migration order.
func storageClassOrder() []config.StorageClass {
	classes := make([]config.StorageClass, 0, len(infraMigrationClasses)+1)
	classes = append(classes, config.StorageClassWork)
	return append(classes, infraMigrationClasses...)
}
