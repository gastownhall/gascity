import { cleanup, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';
import { NowProvider } from '../contexts/NowContext';
import { reportClientError } from '../lib/clientErrorReporting';
import { AgentDetailPage } from './AgentDetail';

vi.mock('../api/client', () => ({
  ApiClientError: class extends Error {
    status: number;
    kind: string | undefined;
    constructor(status: number, message: string, kind?: string) {
      super(message);
      this.status = status;
      this.kind = kind;
    }
  },
  formatApiError: (err: unknown, fallback = 'request failed') => {
    if (err instanceof Error && 'status' in err) {
      const apiErr = err as Error & { status: number };
      return `${apiErr.status} ${apiErr.message}`;
    }
    if (err instanceof Error) return err.message;
    return fallback;
  },
}));

const mockListSupervisorSessions = vi.hoisted(() => vi.fn());
const mockListSupervisorBeads = vi.hoisted(() => vi.fn());
const mockListSupervisorMail = vi.hoisted(() => vi.fn());
const mockUseVisibleRefresh = vi.hoisted(() => vi.fn());

vi.mock('../supervisor/sessionReads', () => ({
  listSupervisorSessions: mockListSupervisorSessions,
  fetchSupervisorSessionTranscript: vi.fn(async () => ({
    turns: [],
    total_chars: 0,
    captured_at: '2026-06-01T00:00:00Z',
    truncated: false,
  })),
  // The live peek now attempts a structured transcript first; null routes it to
  // the conversation fallback above (this suite asserts the page chrome, not the
  // transcript body).
  fetchStructuredTranscript: vi.fn(async () => null),
}));

vi.mock('../supervisor/beadReads', () => ({
  listSupervisorBeadsAssignedTo: mockListSupervisorBeads,
}));

vi.mock('../supervisor/mailReads', () => ({
  listSupervisorMail: mockListSupervisorMail,
}));

vi.mock('../contexts/ViewingAsContext', () => ({
  useViewingAs: () => ({
    viewingAs: { alias: 'stephanie', isOperator: true },
  }),
}));

vi.mock('../hooks/useEntityLinks', () => ({
  useEntityLinks: () => ({
    view: null,
    loading: false,
    error: null,
    refresh: vi.fn(),
  }),
}));

vi.mock('../hooks/useGcEvents', () => ({
  useGcEventRefresh: vi.fn(),
}));

vi.mock('../hooks/useVisibleInterval', () => ({
  useVisibleInterval: vi.fn(),
}));

vi.mock('../hooks/useVisibleRefresh', () => ({
  useVisibleRefresh: mockUseVisibleRefresh,
}));

vi.mock('../lib/clientErrorReporting', () => ({
  reportClientError: vi.fn(),
}));

const mockReportClientError = reportClientError as Mock;

describe('AgentDetailPage error reporting', () => {
  beforeEach(() => {
    mockListSupervisorSessions.mockResolvedValue({ items: [] });
    mockListSupervisorBeads.mockRejectedValue(new Error('beads unavailable'));
    mockListSupervisorMail.mockResolvedValue({ items: [] });
    mockReportClientError.mockReset();
    mockUseVisibleRefresh.mockClear();
  });

  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
  });

  it('reports assigned-bead refresh failures instead of silently dropping them', async () => {
    mockListSupervisorSessions.mockResolvedValue({
      items: [
        {
          id: 'gc-session-1',
          session_name: 'mayor',
          alias: 'mayor',
          template: 'mayor',
          title: 'mayor',
          state: 'active',
          provider: 'claude',
          running: true,
          attached: false,
          created_at: '2026-06-01T00:00:00Z',
        },
      ],
    });

    render(
      <MemoryRouter
        initialEntries={['/agents/mayor']}
        future={{ v7_relativeSplatPath: true, v7_startTransition: true }}
      >
        <NowProvider intervalMs={1_000_000}>
          <Routes>
            <Route path="/agents/:slug" element={<AgentDetailPage />} />
          </Routes>
        </NowProvider>
      </MemoryRouter>,
    );

    await waitFor(() => {
      expect(mockListSupervisorBeads).toHaveBeenCalledWith(['mayor', 'mayor', 'gc-session-1'], {
        includeClosed: true,
      });
      expect(mockReportClientError).toHaveBeenCalledWith({
        component: 'AgentDetail',
        operation: 'refreshBeads',
        message: 'beads unavailable',
      });
    });
    expect(await screen.findByText('beads unavailable')).toBeTruthy();
    expect(screen.queryByText('Loading beads.')).toBeNull();
  });

  it('uses supervisor SSE rather than visible polling for session and bead refreshes', async () => {
    mockListSupervisorSessions.mockResolvedValue({
      items: [
        {
          id: 'gc-session-1',
          session_name: 'mayor',
          alias: 'mayor',
          template: 'mayor',
          title: 'mayor',
          state: 'active',
          provider: 'claude',
          running: true,
          attached: false,
          created_at: '2026-06-01T00:00:00Z',
        },
      ],
    });
    mockListSupervisorBeads.mockResolvedValue({ items: [] });

    render(
      <MemoryRouter
        initialEntries={['/agents/mayor']}
        future={{ v7_relativeSplatPath: true, v7_startTransition: true }}
      >
        <NowProvider intervalMs={1_000_000}>
          <Routes>
            <Route path="/agents/:slug" element={<AgentDetailPage />} />
          </Routes>
        </NowProvider>
      </MemoryRouter>,
    );

    await waitFor(() => {
      expect(mockListSupervisorSessions).toHaveBeenCalled();
      expect(mockListSupervisorBeads).toHaveBeenCalled();
    });
    expect(mockUseVisibleRefresh).not.toHaveBeenCalled();
  });
});
