package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// This file is observe.v1's batched `inventory`: one fork lists every box
// the backend holds for this city, live or dead, with its attributes. On a
// v2-ready pack it is the listing (ListRunning, attested) and the batched
// inventory the v2 lane reads; otherwise neither is served. Like `observe`,
// its stdout can carry GC_INSTANCE_TOKEN, so no error echoes it.

// inventoryOutputCap is the most stdout one `inventory` answer may print.
const inventoryOutputCap = 64 << 20

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
// read (not an object, no name, an empty, non-string or repeated one) fails
// the whole answer, because completeness can no longer be proven. An entry
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
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil || json.Unmarshal(fields["name"], &name) != nil || name == "" {
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

// inventoryEntry maps one listed session's attributes (§4.5). Without a
// readable state of running or dead and its object, or with a dead box
// whose agent lives, it has none. Each other attribute that is null,
// missing or malformed is unknown on its own.
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
	if state != stateRunning && state != stateDead || state == stateDead && agent == agentAlive {
		return runtime.InventoryEntry{}, false
	}
	e := runtime.InventoryEntry{Incarnation: object.ID + "@" + object.Created, DeadKnown: true, AllPanesDead: state == stateDead}
	if state == stateRunning && (agent == agentAlive || agent == agentDead) {
		e.ProcessKnown, e.ProcessAlive = true, agent == agentAlive
	}
	var attached *bool
	if json.Unmarshal(fields["attached"], &attached) == nil && attached != nil {
		e.AttachedKnown, e.Attached = true, *attached
	}
	var activity *string
	if json.Unmarshal(fields["last_activity"], &activity) == nil && activity != nil {
		if at, err := time.Parse(time.RFC3339, *activity); err == nil {
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

// listInventory is ListRunning on a v2-ready pack: the inventory's names
// under prefix, a dead box's included, so an attested listing never proves
// a held name gone. An incomplete inventory returns its names with a
// [runtime.PartialListError]; a failed one returns none.
func (p *Provider) listInventory(prefix string) ([]string, error) {
	l, err := p.inventory(context.Background())
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, name := range l.names {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	if !l.complete {
		return names, &runtime.PartialListError{Err: errors.New("exec provider " + p.script + " inventory: the listing is incomplete")}
	}
	return names, nil
}
