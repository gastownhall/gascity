import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi, type Mock } from 'vitest';
import { useBeadDetail, type BeadDetailState } from '../hooks/useBeadDetail';
import { useEntityLinks } from '../hooks/useEntityLinks';
import { BeadDetailModal } from './BeadDetailModal';

vi.mock('../hooks/useBeadDetail', () => ({
  useBeadDetail: vi.fn(),
}));

vi.mock('../hooks/useEntityLinks', () => ({
  useEntityLinks: vi.fn(() => ({
    view: null,
    loading: false,
    error: null,
  })),
}));

const mockUseBeadDetail = useBeadDetail as unknown as Mock;
const mockUseEntityLinks = useEntityLinks as unknown as Mock;

function detailState(overrides: Partial<BeadDetailState>): BeadDetailState {
  return {
    bead: null,
    loading: false,
    error: null,
    notFound: false,
    now: 1_700_000_000_000,
    ...overrides,
  };
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  mockUseEntityLinks.mockReturnValue({ view: null, loading: false, error: null });
});

describe('BeadDetailModal', () => {
  it('renders a calm resolved-or-removed state for a stale deep-link (404)', () => {
    mockUseBeadDetail.mockReturnValue(detailState({ notFound: true }));

    render(<BeadDetailModal open onClose={() => {}} beadId="gc-316879" />);

    expect(screen.getByText(/resolved or removed/i)).toBeTruthy();
    // Never the hard "Bead not found in the supervisor." error, and never the
    // maroon accent for what is an expected absence.
    expect(screen.queryByText(/not found in the supervisor/i)).toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('still surfaces genuine errors via the alert role', () => {
    mockUseBeadDetail.mockReturnValue(detailState({ error: '500 supervisor down' }));

    render(<BeadDetailModal open onClose={() => {}} beadId="gc-1" />);

    expect(screen.getByRole('alert').textContent).toContain('supervisor down');
  });
});
