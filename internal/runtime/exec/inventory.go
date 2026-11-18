package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/gastownhall/gascity/internal/runtime"
)

// This file is observe.v1's batched `inventory`: one fork lists every box
// the backend holds for this city, live or dead, with its attributes. On a
// v2-ready pack it is the listing (ListRunning, attested) and the batched
// inventory the v2 lane reads; otherwise neither is served. Like `observe`,
// its stdout can carry GC_INSTANCE_TOKEN, so no error echoes it.

// inventoryOutputCap is the most stdout one `inventory` answer may print.
// An entry with every attribute set is well under 1 KiB, so 8 MiB lists
// several thousand boxes, far past a city's fleet, while bounding what one
// misbehaving script makes gc buffer and parse on every pass.
const inventoryOutputCap = 8 << 20

// inventoryListing is a well-formed `inventory` answer.
type inventoryListing struct {
	// complete says the listing is every box the backend holds; otherwise
	// the names listed are present and nothing else is proven absent.
	complete bool
	// names are every listed name, live, dead or with unreadable
	// attributes, in the script's order.
	names []string
	// entries are the attributes of each name whose state and object were
	// readable; a listed name without one has unknown attributes.
	entries map[string]runtime.InventoryEntry
}

// parseInventory reads one `inventory` answer: exactly one object with a
// boolean `complete` and a `sessions` list. An entry whose name cannot be
// read (not an object; no name; an empty, non-string or repeated one; one
// with surrounding whitespace or a control character) fails the whole answer, because completeness can no longer be proven. An entry
// with a readable name but malformed attributes is listed with them
// unknown. No error carries the answer's bytes.
func parseInventory(out []byte) (inventoryListing, error) {
	dec := json.NewDecoder(bytes.NewReader(out))
	var w struct {
		Complete *bool              `json:"complete"`
		Sessions *[]json.RawMessage `json:"sessions"`
	}
	if err := dec.Decode(&w); err != nil {
		return inventoryListing{}, fmt.Errorf("malformed answer (%d bytes)", len(out))
	}
	if _, err := dec.Token(); err != io.EOF {
		return inventoryListing{}, fmt.Errorf("trailing bytes after the answer (%d bytes)", len(out))
	}
	if w.Complete == nil || w.Sessions == nil {
		return inventoryListing{}, fmt.Errorf("an answer without complete or sessions (%d bytes)", len(out))
	}
	l := inventoryListing{complete: *w.Complete, names: make([]string, 0, len(*w.Sessions)), entries: make(map[string]runtime.InventoryEntry, len(*w.Sessions))}
	seen := make(map[string]bool, len(*w.Sessions))
	for i, raw := range *w.Sessions {
		var fields map[string]json.RawMessage
		var name string
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil || json.Unmarshal(fields["name"], &name) != nil || !readableName(name) {
			return inventoryListing{}, fmt.Errorf("session %d has no readable name", i)
		}
		if seen[name] {
			return inventoryListing{}, fmt.Errorf("session %d repeats a listed name", i)
		}
		seen[name] = true
		l.names = append(l.names, name)
		if e, ok := inventoryEntry(fields); ok {
			l.entries[name] = e
		}
	}
	return l, nil
}

// readableName reports a session name gc can list: non-empty, with no
// surrounding whitespace and no control character, so it cannot alias
// another name once trimmed or printed.
func readableName(name string) bool {
	return name != "" && name == strings.TrimSpace(name) && !strings.ContainsFunc(name, unicode.IsControl)
}

// inventoryEntry maps one listed session's attributes (§4.5). Without a
// readable state of running or dead and its object, or with a dead box
// whose agent lives, it has none. Its incarnation is the object,
// "<id>:<created>", so a later `observe` of the same box matches it. Each
// other attribute that is null, missing or malformed is unknown on its
// own, and so is a last_activity at or before the epoch (a Go adapter's
// zero time.Time), as tmux reads an activity of 0.
func inventoryEntry(fields map[string]json.RawMessage) (runtime.InventoryEntry, bool) {
	var state, agent string
	var object observeObject
	if json.Unmarshal(fields["state"], &state) != nil || json.Unmarshal(fields["object"], &object) != nil ||
		strings.TrimSpace(object.ID) == "" || strings.TrimSpace(object.Created) == "" {
		return runtime.InventoryEntry{}, false
	}
	if fields["agent"] != nil && json.Unmarshal(fields["agent"], &agent) != nil {
		agent = ""
	}
	if state != stateRunning && state != stateDead {
		return runtime.InventoryEntry{}, false
	}
	if state == stateDead && agent == agentAlive {
		return runtime.InventoryEntry{}, false
	}
	e := runtime.InventoryEntry{Incarnation: object.ID + ":" + object.Created, DeadKnown: true, AllPanesDead: state == stateDead}
	if state == stateRunning && (agent == agentAlive || agent == agentDead) {
		e.ProcessKnown, e.ProcessAlive = true, agent == agentAlive
	}
	var attached *bool
	if json.Unmarshal(fields["attached"], &attached) == nil && attached != nil {
		e.AttachedKnown, e.Attached = true, *attached
	}
	var activity *string
	if json.Unmarshal(fields["last_activity"], &activity) == nil && activity != nil {
		if at, err := time.Parse(time.RFC3339, *activity); err == nil && at.Unix() > 0 {
			e.ActivityKnown, e.LastActivity = true, at
		}
	}
	var restart *bool
	if json.Unmarshal(fields["restart_requested"], &restart) == nil && restart != nil {
		e.RestartRequested = *restart
	}
	var identity map[string]string
	if json.Unmarshal(fields["identity"], &identity) == nil && identity != nil {
		e.IdentityKnown, e.Identity = true, identity
	}
	return e, true
}

// inventory runs `inventory` under ctx and the provider's op timeout, with
// stdout capped at inventoryOutputCap. Any failure, exit 2 included, is an
// error: never an empty listing.
func (p *Provider) inventory(ctx context.Context) (inventoryListing, error) {
	if !p.v2Ready() {
		return inventoryListing{}, fmt.Errorf("%w: exec provider %s: an inventory needs %s", runtime.ErrRuntimeUnavailable, p.script, runtime.ProtocolCapabilityObserveV1)
	}
	out, err := p.runCapped(ctx, p.timeout, inventoryOutputCap, nil, "inventory")
	if err != nil {
		return inventoryListing{}, err
	}
	l, err := parseInventory([]byte(out))
	if err != nil {
		return inventoryListing{}, fmt.Errorf("exec provider %s inventory: %w", p.script, err)
	}
	if l.complete {
		p.dropAbsentPrograms(l.names)
	}
	return l, nil
}

// RuntimeInventory implements [runtime.InventoryProvider] with one
// `inventory` under ctx. A pack that is not v2-ready serves none (see
// InventoryAvailable); its error wraps [runtime.ErrRuntimeUnavailable].
func (p *Provider) RuntimeInventory(ctx context.Context) (map[string]runtime.InventoryEntry, error) {
	l, err := p.inventory(ctx)
	if err != nil {
		return nil, err
	}
	return l.entries, nil
}

// InventoryAvailable implements [runtime.InventoryGate]: a v2-ready pack
// serves its inventory. Safe on a nil provider.
func (p *Provider) InventoryAvailable() bool {
	return p.v2Ready()
}

// ListRunningComplete implements [runtime.ListingAttestation]: a v2-ready
// pack's listing is its `inventory`, which lists every box, live or dead,
// and says when it is partial. Safe on a nil provider.
func (p *Provider) ListRunningComplete() bool {
	return p.v2Ready()
}

// ListInventory implements [runtime.InventoryLister]: one `inventory`
// under the provider's op timeout answers ListRunning(prefix) and the
// inventory of the names it lists. The names are every listed name under
// prefix, a dead box's included, so an attested listing never proves a
// held name gone. An incomplete inventory returns them with a
// [runtime.PartialListError]; a failed one returns none.
func (p *Provider) ListInventory(prefix string) ([]string, map[string]runtime.InventoryEntry, error) {
	return p.listInventory(prefix, true)
}

func (p *Provider) listInventory(prefix string, poll bool) ([]string, map[string]runtime.InventoryEntry, error) {
	l, err := p.inventory(context.Background())
	if err != nil {
		return nil, nil, err
	}
	names := []string{}
	entries := make(map[string]runtime.InventoryEntry)
	for _, name := range l.names {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		names = append(names, name)
		if e, ok := l.entries[name]; ok {
			entries[name] = e
		}
	}
	if poll {
		p.pollProgramStatus(entries)
	}
	if !l.complete {
		return names, entries, &runtime.PartialListError{Err: errors.New("exec provider " + p.script + " inventory: the listing is incomplete")}
	}
	return names, entries, nil
}

var (
	programStatusPollWorkers = 8
	programStatusPollBound   = 3 * time.Second
)

func (p *Provider) dropAbsentPrograms(names []string) {
	present := make(map[string]bool, len(names))
	for _, name := range names {
		present[name] = true
	}
	p.programs.mu.Lock()
	defer p.programs.mu.Unlock()
	for name := range p.programs.sessions {
		if !present[name] {
			delete(p.programs.sessions, name)
		}
	}
}

func (p *Provider) pollProgramStatus(entries map[string]runtime.InventoryEntry) {
	if p.programStatusMode() == runtime.ProgramStatusOff || !p.handshakeCapability(runtime.ProtocolCapabilityProgramStatus) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), programStatusPollBound)
	defer cancel()
	p.pollProgramStatusRead(ctx, entries, p.readProgramStatus)
}

// pollProgramStatusRead owns the bounded pool and rotation; the function value
// lets timing tests exercise the same pool without a subprocess clock.
func (p *Provider) pollProgramStatusRead(ctx context.Context, entries map[string]runtime.InventoryEntry, read func(context.Context, string, time.Duration) (runtime.ProgramStatus, error)) {
	type candidate struct {
		name string
		at   time.Time
	}
	var candidates []candidate
	p.programs.mu.Lock()
	for name, entry := range entries {
		if entry.DeadKnown && !entry.AllPanesDead {
			e := p.programSessionLocked(name)
			candidates = append(candidates, candidate{name, e.lastPoll})
		}
	}
	p.programs.mu.Unlock()
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].at.Equal(candidates[j].at) {
			return candidates[i].name < candidates[j].name
		}
		return candidates[i].at.Before(candidates[j].at)
	})
	type result struct {
		name   string
		status runtime.ProgramStatus
		err    error
	}
	results := make(chan result, len(candidates))
	jobs := make(chan string)
	for range min(programStatusPollWorkers, len(candidates)) {
		go func() {
			for name := range jobs {
				if ctx.Err() != nil {
					continue
				}
				p.programs.mu.Lock()
				e := p.programSessionLocked(name)
				attemptedAt := programStatusNow()
				p.programs.mu.Unlock()
				status, err := read(ctx, name, programStatusReadTimeout)
				// A read cut short by the total budget keeps its priority.
				// Otherwise a fixed final wave can time out every pass.
				if ctx.Err() == nil {
					p.programs.mu.Lock()
					if p.programs.sessions[name] == e {
						e.lastPoll = attemptedAt
					}
					p.programs.mu.Unlock()
				}
				results <- result{name, status, err}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, c := range candidates {
			select {
			case jobs <- c.name:
			case <-ctx.Done():
				return
			}
		}
	}()
	apply := func(r result) {
		if r.err == nil {
			e := entries[r.name]
			e.Program = r.status
			entries[r.name] = e
		}
	}
	for range candidates {
		select {
		case r := <-results:
			apply(r)
		case <-ctx.Done():
			// Keep completed outcomes already queued before the deadline.
			// Late reads are discarded by readProgramStatus's parent fence.
			for {
				select {
				case r := <-results:
					apply(r)
				default:
					return
				}
			}
		}
	}
}

// Inventory is one well-formed `inventory` answer as gc reads it: every
// listed name, and the attributes of each name whose state and object were
// readable (runtime.InventoryEntry; Incarnation is "<id>:<created>"). It is
// exported for the conformance kit.
type Inventory struct {
	Complete bool
	Names    []string
	Entries  map[string]runtime.InventoryEntry
}

// ParseInventory reads one `inventory` answer as the provider does: an
// answer it would refuse (an unreadable or repeated name, trailing bytes,
// no complete flag) is an error.
func ParseInventory(out []byte) (Inventory, error) {
	l, err := parseInventory(out)
	if err != nil {
		return Inventory{}, err
	}
	return Inventory{Complete: l.complete, Names: l.names, Entries: l.entries}, nil
}
