import { errorMessage, GC_EVENT_PREFIX } from 'gas-city-dashboard-shared';
import { useCallback, useEffect, useMemo, useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { formatApiError } from '../api/client';
import { BeadDetailModal } from '../components/BeadDetailModal';
import { Button } from '../components/Button';
import { PageHeader } from '../components/PageHeader';
import { RelatedEntities } from '../components/RelatedEntities';
import { StatusBadge, stateTone } from '../components/StatusBadge';
import { AgentBeadsAssigned } from '../components/agent/AgentBeadsAssigned';
import { AgentChatThread } from '../components/agent/AgentChatThread';
import { AgentLivePeek } from '../components/agent/AgentLivePeek';
import { AgentMetadata } from '../components/agent/AgentMetadata';
import { useOperatorConfig } from '../contexts/OperatorConfigContext';
import { useViewingAs } from '../contexts/ViewingAsContext';
import { useNow } from '../contexts/NowContext';
import { useAbortableVisibleRefresh } from '../hooks/useAbortableVisibleRefresh';
import { useEntityLinks } from '../hooks/useEntityLinks';
import { useGcEventRefresh } from '../hooks/useGcEvents';
import { reportClientError } from '../lib/clientErrorReporting';
import { listSupervisorBeadsAssignedTo, type SupervisorBead } from '../supervisor/beadReads';
import { listSupervisorMail, type SupervisorMailItem } from '../supervisor/mailReads';
import { listSupervisorSessions, type SupervisorSession } from '../supervisor/sessionReads';

// Read-only drilldown for a single agent. Route: /agents/:slug where
// slug resolves against session_name, alias, then id (see sessionSlug).
//
// Surface:
//   - Header with state badge, identity line, back link
//   - Metadata block (rig, pool, template, model, ctx, attached, timestamps)
//   - Beads assigned to this agent (filtered from direct supervisor reads)
//   - Live peek panel (live SSE stream for active sessions; snapshot otherwise)
//
// Read-only scope: nudge actions, chat compose, and directive edits stay
// out of this route until the backend exposes explicit write endpoints.

const CHAT_REFRESH_MS = 10_000;
const CHAT_MAX_MESSAGES = 200;
export function AgentDetailPage() {
  const { slug = '' } = useParams<{ slug: string }>();
  const navigate = useNavigate();
  const { viewingAs } = useViewingAs();
  const operator = useOperatorConfig();

  const [sessions, setSessions] = useState<SupervisorSession[] | null>(null);
  const [beads, setBeads] = useState<SupervisorBead[] | null>(null);
  const [beadsError, setBeadsError] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [viewingBead, setViewingBead] = useState<SupervisorBead | null>(null);
  const [viewingBeadId, setViewingBeadId] = useState<string | null>(null);

  const now = useNow();

  const decoded = useMemo(() => {
    try {
      return decodeURIComponent(slug);
    } catch (err) {
      void reportClientError({
        component: 'AgentDetail',
        operation: 'decodeSlug',
        message: errorMessage(err),
      });
      return slug;
    }
  }, [slug]);

  const refreshSessions = useCallback(async () => {
    try {
      const { items } = await listSupervisorSessions();
      setSessions(items ?? []);
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'sessions failed');
    }
  }, []);

  const session = useMemo<SupervisorSession | null>(() => {
    if (sessions === null) return null;
    return (
      sessions.find((s) => s.session_name === decoded) ??
      sessions.find((s) => s.alias === decoded) ??
      sessions.find((s) => s.id === decoded) ??
      null
    );
  }, [sessions, decoded]);

  const beadAssignees = useMemo<readonly string[]>(() => {
    if (session === null) return [];
    return [session.alias ?? '', session.session_name, session.id];
  }, [session]);

  const refreshBeads = useCallback(async () => {
    if (beadAssignees.length === 0) {
      setBeads([]);
      setBeadsError(null);
      return;
    }
    try {
      const { items } = await listSupervisorBeadsAssignedTo(beadAssignees, {
        includeClosed: true,
      });
      setBeads(items);
      setBeadsError(null);
    } catch (err) {
      setBeads([]);
      setBeadsError(formatApiError(err, 'assigned beads unavailable'));
      void reportClientError({
        component: 'AgentDetail',
        operation: 'refreshBeads',
        message: errorMessage(err),
      });
    }
  }, [beadAssignees]);

  useEffect(() => {
    void refreshSessions();
  }, [refreshSessions]);

  useEffect(() => {
    void refreshBeads();
  }, [refreshBeads]);

  // SSE is the freshness channel for supervisor session/bead changes.
  useGcEventRefresh([GC_EVENT_PREFIX.session, GC_EVENT_PREFIX.bead], () => {
    void refreshSessions();
    void refreshBeads();
  });

  // Beads belonging to this agent. Two link paths in circulation, both
  // need to be matched or sessions with an empty alias and work linked only
  // through metadata.session_id silently render empty:
  //   1. bead.assignee == alias | session_name | id
  //   2. bead.metadata.session_id == session.id (supervisor-spawned)
  //   3. bead.metadata.session_name == session.session_name (CLI-tagged)
  const assignedBeads = useMemo<SupervisorBead[]>(() => {
    if (session === null || beads === null) return [];
    const candidates = new Set<string>();
    if (session.alias) candidates.add(session.alias);
    if (session.session_name) candidates.add(session.session_name);
    candidates.add(session.id);
    return beads.filter((b) => {
      if (b.assignee !== undefined && candidates.has(b.assignee)) return true;
      // Supervisor Bead metadata is Record<string, string> per OpenAPI (F11)
      // and session_name is required (F10), so neither check needs the
      // prior runtime guards.
      const md = b.metadata;
      if (md) {
        if (md.session_id === session.id) return true;
        if (md.session_name && md.session_name === session.session_name) {
          return true;
        }
      }
      return false;
    });
  }, [session, beads]);

  // Chat thread: wide supervisor mail fetch, client-side filter for
  // messages between operator and this agent.
  const agentAliases = useMemo<ReadonlyArray<string>>(() => {
    if (session === null) return [];
    const out = new Set<string>();
    if (session.alias) out.add(session.alias.toLowerCase());
    if (session.session_name) out.add(session.session_name.toLowerCase());
    out.add(session.id.toLowerCase());
    return [...out];
  }, [session]);

  const operatorAliases = useMemo<ReadonlyArray<string>>(
    () => [viewingAs.alias.toLowerCase(), operator.operatorWireAlias.toLowerCase()],
    [viewingAs.alias, operator.operatorWireAlias],
  );

  const loadChatItems = useCallback(async (): Promise<SupervisorMailItem[]> => {
    const { items } = await listSupervisorMail('all', viewingAs.alias, operator);
    return items;
  }, [viewingAs.alias, operator]);

  const chatState = useAbortableVisibleRefresh({
    enabled: session !== null,
    intervalMs: CHAT_REFRESH_MS,
    load: loadChatItems,
    formatError: formatApiError,
  });

  const chatLoading = chatState.status === 'loading';
  const chatError =
    chatState.status === 'failed'
      ? chatState.error
      : chatState.status === 'ready' && chatState.error.length > 0
        ? chatState.error
        : null;

  // Related entities (gascity-dashboard-j4x). Focus on the session id so
  // the index surfaces the beads, formula runs, and PRs adjacent to this
  // agent's work. Hook is called unconditionally (before the early
  // returns) per rules-of-hooks; a null ref leaves it idle.
  const links = useEntityLinks(session?.id ?? null);

  const chatMessages = useMemo<ReadonlyArray<SupervisorMailItem>>(() => {
    const chatItems = chatState.status === 'ready' ? chatState.data : [];
    const agents = new Set(agentAliases);
    const operators = new Set(operatorAliases);
    const filtered = chatItems.filter((m) => {
      const from = (m.from ?? '').toLowerCase();
      const to = (m.to ?? '').toLowerCase();
      // Operator → agent
      if (operators.has(from) && agents.has(to)) return true;
      // Agent → operator
      if (agents.has(from) && operators.has(to)) return true;
      return false;
    });
    filtered.sort((a, b) => a.created_at.localeCompare(b.created_at));
    if (filtered.length > CHAT_MAX_MESSAGES) {
      return filtered.slice(filtered.length - CHAT_MAX_MESSAGES);
    }
    return filtered;
  }, [chatState, agentAliases, operatorAliases]);

  if (sessions === null) {
    return (
      <section>
        <PageHeader title="Agent" synopsis="Loading session list." />
      </section>
    );
  }

  if (session === null) {
    return (
      <section>
        <PageHeader
          title="Agent"
          synopsis={
            <>
              No session matches <code className="text-fg">{decoded}</code>.
            </>
          }
          meta={
            <Button size="sm" tone="quiet" onClick={() => navigate('/agents')}>
              ← Agents
            </Button>
          }
        />
        <p className="text-body text-fg-muted max-w-prose">
          The slug doesn't match any current session's session_name, alias, or id. Sessions are
          listed at{' '}
          <Link to="/agents" className="text-accent hover:underline">
            /agents
          </Link>
          .
        </p>
      </section>
    );
  }

  const label = session.alias ?? session.title ?? session.id;
  const tone = stateTone(session.state);

  const openBead = (id: string) => {
    setViewingBead(null);
    setViewingBeadId(id);
  };
  const closeBead = () => {
    setViewingBead(null);
    setViewingBeadId(null);
  };

  return (
    <section>
      <PageHeader
        title={label}
        synopsis={
          <span className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
            <StatusBadge
              tone={tone}
              label={session.state}
              {...(session.attached ? { trailing: 'att' } : {})}
              {...(session.reason ? { title: `reason: ${session.reason}` } : {})}
            />
            <span className="text-fg-faint">·</span>
            <code className="text-fg-muted">{session.template ?? '—'}</code>
            {session.session_name && session.session_name !== session.alias && (
              <>
                <span className="text-fg-faint">·</span>
                <span className="text-fg-faint">{session.session_name}</span>
              </>
            )}
            <span className="text-fg-faint">·</span>
            <span className="text-fg-faint">
              id <code className="text-fg-muted">{session.id}</code>
            </span>
          </span>
        }
        meta={
          <Link to="/agents">
            <Button size="sm" tone="quiet">
              ← Agents
            </Button>
          </Link>
        }
      />

      {error && (
        <p className="text-body text-accent mb-6" role="alert">
          {error}
        </p>
      )}

      <AgentMetadata session={session} now={now} />

      <AgentBeadsAssigned
        beads={assignedBeads}
        error={beadsError}
        loading={beads === null}
        onSelect={(b) => {
          setViewingBeadId(null);
          setViewingBead(b);
        }}
      />

      <RelatedEntities
        view={links.view}
        loading={links.loading}
        error={links.error}
        now={now}
        onOpenBead={openBead}
      />

      <AgentLivePeek session={session} />

      <AgentChatThread messages={chatMessages} loading={chatLoading} error={chatError} now={now} />

      <BeadDetailModal
        open={viewingBead !== null || viewingBeadId !== null}
        onClose={closeBead}
        beadId={viewingBead?.id ?? viewingBeadId}
        initialBead={viewingBead}
        onOpenBead={openBead}
      />
    </section>
  );
}
