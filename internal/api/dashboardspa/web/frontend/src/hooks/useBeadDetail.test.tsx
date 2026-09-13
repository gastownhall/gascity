import { cleanup, renderHook, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';
import { fetchSupervisorBead } from '../supervisor/beadReads';
import { SupervisorApiError } from '../supervisor/client';
import { useBeadDetail } from './useBeadDetail';

vi.mock('../contexts/NowContext', () => ({
  useNow: () => 1_700_000_000_000,
}));

vi.mock('../supervisor/beadReads', () => ({
  fetchSupervisorBead: vi.fn(),
}));

const mockFetch = fetchSupervisorBead as unknown as Mock;

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe('useBeadDetail', () => {
  beforeEach(() => {
    mockFetch.mockReset();
  });

  it('flags notFound (not error) when the deep-linked bead 404s', async () => {
    mockFetch.mockRejectedValue(new SupervisorApiError(404, 'bead missing', undefined));

    const { result } = renderHook(() => useBeadDetail(true, 'gc-316879'));

    await waitFor(() => expect(result.current.loading).toBe(false));

    // A pruned / since-closed deep-link is a calm absence, not a hard error:
    // notFound true, error stays null so the modal can render the resolved
    // state instead of "Bead not found in the supervisor." (gascity-dashboard-sg9o).
    expect(result.current.notFound).toBe(true);
    expect(result.current.error).toBeNull();
    expect(result.current.bead).toBeNull();
  });

  it('surfaces non-404 failures as a hard error, never as notFound', async () => {
    mockFetch.mockRejectedValue(new SupervisorApiError(500, 'supervisor down', undefined));

    const { result } = renderHook(() => useBeadDetail(true, 'gc-1'));

    await waitFor(() => expect(result.current.loading).toBe(false));

    expect(result.current.notFound).toBe(false);
    expect(result.current.error).toContain('supervisor down');
  });

  it('loads the bead and leaves notFound clear on success', async () => {
    mockFetch.mockResolvedValue({
      id: 'gc-1',
      title: 'live bead',
      issue_type: 'task',
      status: 'open',
      description: 'body',
    });

    const { result } = renderHook(() => useBeadDetail(true, 'gc-1'));

    await waitFor(() => expect(result.current.bead?.id).toBe('gc-1'));

    expect(result.current.notFound).toBe(false);
    expect(result.current.error).toBeNull();
  });

  it('does not fetch when inactive', async () => {
    renderHook(() => useBeadDetail(false, 'gc-1'));
    expect(mockFetch).not.toHaveBeenCalled();
  });
});
