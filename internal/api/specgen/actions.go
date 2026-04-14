package specgen

import (
	"reflect"

	"github.com/gastownhall/gascity/internal/api"
)

// typeOf is a helper to get reflect.Type from a zero value.
func typeOf[T any]() reflect.Type {
	return reflect.TypeOf((*T)(nil)).Elem()
}

// BuildRegistry returns the canonical action registry for the Gas City
// WebSocket API. This is the single source of truth for spec generation.
//
// To add a new action: add a Register call here and the corresponding
// case in websocket.go's handleSocketRequest switch. The sync test
// (TestAsyncAPIActionsMatchGoCode) will catch mismatches.
func BuildRegistry() *Registry {
	r := NewRegistry()

	// ── Health / Status ──────────────────────────────────────────
	r.Register(ActionDef{Action: "health.get", Description: "Health check"})
	r.Register(ActionDef{Action: "status.get", Description: "City status snapshot"})

	// ── City ─────────────────────────────────────────────────────
	r.Register(ActionDef{Action: "cities.list", Description: "List managed cities (supervisor)"})
	r.Register(ActionDef{Action: "city.get", Description: "Get city details"})
	r.Register(ActionDef{Action: "city.patch", Description: "Update city (suspend/resume)", IsMutation: true})

	// ── Config ───────────────────────────────────────────────────
	r.Register(ActionDef{Action: "config.get", Description: "Get parsed city configuration"})
	r.Register(ActionDef{Action: "config.explain", Description: "Explain config resolution"})
	r.Register(ActionDef{Action: "config.validate", Description: "Validate city configuration"})

	// ── Sessions ─────────────────────────────────────────────────
	r.Register(ActionDef{Action: "sessions.list", Description: "List sessions"})
	r.Register(ActionDef{Action: "session.get", Description: "Get session details"})
	r.Register(ActionDef{Action: "session.create", Description: "Create a new session", IsMutation: true})
	r.Register(ActionDef{Action: "session.suspend", Description: "Suspend a session", IsMutation: true})
	r.Register(ActionDef{Action: "session.close", Description: "Close a session", IsMutation: true})
	r.Register(ActionDef{Action: "session.stop", Description: "Stop a session", IsMutation: true})
	r.Register(ActionDef{Action: "session.wake", Description: "Wake a suspended session", IsMutation: true})
	r.Register(ActionDef{Action: "session.rename", Description: "Rename a session", IsMutation: true})
	r.Register(ActionDef{Action: "session.respond", Description: "Respond to a session prompt", IsMutation: true})
	r.Register(ActionDef{Action: "session.kill", Description: "Force-kill a session", IsMutation: true})
	r.Register(ActionDef{Action: "session.pending", Description: "Get pending input requests"})
	r.Register(ActionDef{Action: "session.submit", Description: "Submit work to a session", IsMutation: true})
	r.Register(ActionDef{Action: "session.transcript", Description: "Get session transcript"})
	r.Register(ActionDef{Action: "session.patch", Description: "Update session metadata", IsMutation: true})
	r.Register(ActionDef{Action: "session.messages", Description: "Get session messages"})
	r.Register(ActionDef{Action: "session.agents.list", Description: "List agents in a session"})
	r.Register(ActionDef{Action: "session.agent.get", Description: "Get session agent details"})

	// ── Beads ────────────────────────────────────────────────────
	r.Register(ActionDef{Action: "beads.list", Description: "List beads with filters"})
	r.Register(ActionDef{Action: "beads.ready", Description: "List ready beads"})
	r.Register(ActionDef{Action: "beads.graph", Description: "Get bead dependency graph"})
	r.Register(ActionDef{Action: "bead.get", Description: "Get a bead by ID"})
	r.Register(ActionDef{Action: "bead.deps", Description: "Get bead dependencies"})
	r.Register(ActionDef{Action: "bead.create", Description: "Create a new bead", IsMutation: true})
	r.Register(ActionDef{Action: "bead.close", Description: "Close a bead", IsMutation: true})
	r.Register(ActionDef{Action: "bead.update", Description: "Update a bead", IsMutation: true})
	r.Register(ActionDef{Action: "bead.reopen", Description: "Reopen a closed bead", IsMutation: true})
	r.Register(ActionDef{Action: "bead.assign", Description: "Assign a bead to an agent", IsMutation: true})
	r.Register(ActionDef{Action: "bead.delete", Description: "Delete a bead", IsMutation: true})

	// ── Mail ─────────────────────────────────────────────────────
	r.Register(ActionDef{Action: "mail.list", Description: "List mail messages"})
	r.Register(ActionDef{Action: "mail.get", Description: "Get a mail message"})
	r.Register(ActionDef{Action: "mail.count", Description: "Count mail messages"})
	r.Register(ActionDef{Action: "mail.thread", Description: "Get a mail thread"})
	r.Register(ActionDef{Action: "mail.read", Description: "Mark mail as read", IsMutation: true})
	r.Register(ActionDef{Action: "mail.mark_unread", Description: "Mark mail as unread", IsMutation: true})
	r.Register(ActionDef{Action: "mail.archive", Description: "Archive a mail message", IsMutation: true})
	r.Register(ActionDef{Action: "mail.reply", Description: "Reply to a mail message", IsMutation: true})
	r.Register(ActionDef{Action: "mail.send", Description: "Send a new mail message", IsMutation: true})
	r.Register(ActionDef{Action: "mail.delete", Description: "Delete a mail message", IsMutation: true})

	// ── Events ───────────────────────────────────────────────────
	r.Register(ActionDef{Action: "events.list", Description: "List events with filters"})
	r.Register(ActionDef{Action: "event.emit", Description: "Emit a custom event", IsMutation: true})

	// ── Agents ───────────────────────────────────────────────────
	r.Register(ActionDef{Action: "agents.list", Description: "List agents"})
	r.Register(ActionDef{Action: "agent.get", Description: "Get agent details"})
	r.Register(ActionDef{Action: "agent.suspend", Description: "Suspend an agent", IsMutation: true})
	r.Register(ActionDef{Action: "agent.resume", Description: "Resume a suspended agent", IsMutation: true})
	r.Register(ActionDef{Action: "agent.create", Description: "Create an agent", IsMutation: true})
	r.Register(ActionDef{Action: "agent.update", Description: "Update agent config", IsMutation: true})
	r.Register(ActionDef{Action: "agent.delete", Description: "Delete an agent", IsMutation: true})

	// ── Rigs ─────────────────────────────────────────────────────
	r.Register(ActionDef{Action: "rigs.list", Description: "List rigs"})
	r.Register(ActionDef{Action: "rig.get", Description: "Get rig details"})
	r.Register(ActionDef{Action: "rig.suspend", Description: "Suspend a rig", IsMutation: true})
	r.Register(ActionDef{Action: "rig.resume", Description: "Resume a suspended rig", IsMutation: true})
	r.Register(ActionDef{Action: "rig.restart", Description: "Restart a rig", IsMutation: true})
	r.Register(ActionDef{Action: "rig.create", Description: "Create a rig", IsMutation: true})
	r.Register(ActionDef{Action: "rig.update", Description: "Update rig config", IsMutation: true})
	r.Register(ActionDef{Action: "rig.delete", Description: "Delete a rig", IsMutation: true})

	// ── Convoys ──────────────────────────────────────────────────
	r.Register(ActionDef{Action: "convoys.list", Description: "List convoys"})
	r.Register(ActionDef{Action: "convoy.get", Description: "Get convoy details"})
	r.Register(ActionDef{Action: "convoy.create", Description: "Create a convoy", IsMutation: true})
	r.Register(ActionDef{Action: "convoy.add", Description: "Add items to a convoy", IsMutation: true})
	r.Register(ActionDef{Action: "convoy.remove", Description: "Remove items from a convoy", IsMutation: true})
	r.Register(ActionDef{Action: "convoy.check", Description: "Check convoy status"})
	r.Register(ActionDef{Action: "convoy.close", Description: "Close a convoy", IsMutation: true})
	r.Register(ActionDef{Action: "convoy.delete", Description: "Delete a convoy", IsMutation: true})

	// ── Services ─────────────────────────────────────────────────
	r.Register(ActionDef{Action: "services.list", Description: "List workspace services"})
	r.Register(ActionDef{Action: "service.get", Description: "Get service status"})
	r.Register(ActionDef{Action: "service.restart", Description: "Restart a service", IsMutation: true})

	// ── Providers ────────────────────────────────────────────────
	r.Register(ActionDef{Action: "providers.list", Description: "List AI providers"})
	r.Register(ActionDef{Action: "provider.get", Description: "Get provider details"})
	r.Register(ActionDef{Action: "provider.create", Description: "Create a provider", IsMutation: true})
	r.Register(ActionDef{Action: "provider.update", Description: "Update provider config", IsMutation: true})
	r.Register(ActionDef{Action: "provider.delete", Description: "Delete a provider", IsMutation: true})

	// ── Formulas ─────────────────────────────────────────────────
	r.Register(ActionDef{Action: "formulas.list", Description: "List formulas"})
	r.Register(ActionDef{Action: "formulas.feed", Description: "Formula activity feed"})
	r.Register(ActionDef{Action: "formula.get", Description: "Get formula details"})
	r.Register(ActionDef{Action: "formula.runs", Description: "Get formula run history"})

	// ── Orders ───────────────────────────────────────────────────
	r.Register(ActionDef{Action: "orders.list", Description: "List orders"})
	r.Register(ActionDef{Action: "orders.check", Description: "Check order gate conditions"})
	r.Register(ActionDef{Action: "orders.history", Description: "Get order history"})
	r.Register(ActionDef{Action: "orders.feed", Description: "Order activity feed"})
	r.Register(ActionDef{Action: "order.get", Description: "Get order details"})
	r.Register(ActionDef{Action: "order.enable", Description: "Enable an order", IsMutation: true})
	r.Register(ActionDef{Action: "order.disable", Description: "Disable an order", IsMutation: true})
	r.Register(ActionDef{Action: "order.history.detail", Description: "Get order history detail"})

	// ── Packs ────────────────────────────────────────────────────
	r.Register(ActionDef{Action: "packs.list", Description: "List installed packs"})

	// ── Sling (dispatch) ──────────────────────────────────────���──
	r.Register(ActionDef{Action: "sling.run", Description: "Run sling dispatch", IsMutation: true})

	// ── External messaging ───────────────────────────────────────
	r.Register(ActionDef{Action: "extmsg.inbound", Description: "Process inbound external message", IsMutation: true})
	r.Register(ActionDef{Action: "extmsg.outbound", Description: "Send outbound external message", IsMutation: true})
	r.Register(ActionDef{Action: "extmsg.bindings.list", Description: "List external message bindings"})
	r.Register(ActionDef{Action: "extmsg.bind", Description: "Bind external messaging channel", IsMutation: true})
	r.Register(ActionDef{Action: "extmsg.unbind", Description: "Unbind external messaging channel", IsMutation: true})
	r.Register(ActionDef{Action: "extmsg.groups.lookup", Description: "Look up messaging groups"})
	r.Register(ActionDef{Action: "extmsg.groups.ensure", Description: "Ensure messaging group exists", IsMutation: true})
	r.Register(ActionDef{Action: "extmsg.participant.upsert", Description: "Add/update messaging participant", IsMutation: true})
	r.Register(ActionDef{Action: "extmsg.participant.remove", Description: "Remove messaging participant", IsMutation: true})
	r.Register(ActionDef{Action: "extmsg.transcript.list", Description: "List external message transcript"})
	r.Register(ActionDef{Action: "extmsg.transcript.ack", Description: "Acknowledge transcript messages", IsMutation: true})
	r.Register(ActionDef{Action: "extmsg.adapters.list", Description: "List messaging adapters"})
	r.Register(ActionDef{Action: "extmsg.adapters.register", Description: "Register messaging adapter", IsMutation: true})
	r.Register(ActionDef{Action: "extmsg.adapters.unregister", Description: "Unregister messaging adapter", IsMutation: true})

	// ── Patches (config overrides) ───────────────────────────────
	r.Register(ActionDef{Action: "patches.agents.list", Description: "List agent patches"})
	r.Register(ActionDef{Action: "patches.agent.get", Description: "Get agent patch"})
	r.Register(ActionDef{Action: "patches.agents.set", Description: "Set agent patch", IsMutation: true})
	r.Register(ActionDef{Action: "patches.agent.delete", Description: "Delete agent patch", IsMutation: true})
	r.Register(ActionDef{Action: "patches.rigs.list", Description: "List rig patches"})
	r.Register(ActionDef{Action: "patches.rig.get", Description: "Get rig patch"})
	r.Register(ActionDef{Action: "patches.rigs.set", Description: "Set rig patch", IsMutation: true})
	r.Register(ActionDef{Action: "patches.rig.delete", Description: "Delete rig patch", IsMutation: true})
	r.Register(ActionDef{Action: "patches.providers.list", Description: "List provider patches"})
	r.Register(ActionDef{Action: "patches.provider.get", Description: "Get provider patch"})
	r.Register(ActionDef{Action: "patches.providers.set", Description: "Set provider patch", IsMutation: true})
	r.Register(ActionDef{Action: "patches.provider.delete", Description: "Delete provider patch", IsMutation: true})

	// ── Workflows ────────────────────────────────────────────────
	r.Register(ActionDef{Action: "workflow.get", Description: "Get workflow details"})
	r.Register(ActionDef{Action: "workflow.delete", Description: "Delete a workflow", IsMutation: true})

	// ── Subscriptions (protocol-level) ───────────────────────────
	r.Register(ActionDef{Action: "subscription.start", Description: "Start an event or session stream subscription"})
	r.Register(ActionDef{Action: "subscription.stop", Description: "Stop a subscription"})

	return r
}

// Ensure the api package is imported (needed for types).
var _ = api.IsConnError
