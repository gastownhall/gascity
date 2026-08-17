import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { BeadStatus } from 'gas-city-dashboard-shared';
import type { SupervisorBead } from '../../supervisor/beadReads';
import { buildBeadGraph } from '../../lib/beadGraph';
import { assertAtMostOneMark } from '../../test/assertions/oneMarkRule';
import { BeadBoard } from './BeadBoard';

const scrollIntoView = vi.fn();

beforeEach(() => {
  scrollIntoView.mockClear();
  Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', {
    configurable: true,
    value: scrollIntoView,
  });
});

afterEach(() => cleanup());

function bead(id: string, status: BeadStatus, extra: Partial<SupervisorBead> = {}): SupervisorBead {
  return {
    id,
    title: `bead ${id}`,
    status,
    issue_type: 'task',
    created_at: '2026-05-01T00:00:00Z',
    ...extra,
  };
}

function renderBoard(
  beads: SupervisorBead[],
  selectedId: string | null = null,
  onSelect = vi.fn(),
) {
  const graph = buildBeadGraph(beads);
  const result = render(
    <BeadBoard columns={graph.columns} selectedId={selectedId} onSelect={onSelect} />,
  );
  return { ...result, onSelect };
}

describe('BeadBoard', () => {
  it('renders every status column as a labelled section', () => {
    renderBoard([bead('A', 'open')]);
    for (const label of ['ready', 'open', 'in progress', 'blocked', 'done']) {
      expect(screen.getByRole('region', { name: label })).toBeTruthy();
    }
  });

  it('places a bead under the column its status maps to', () => {
    renderBoard([
      bead('A', 'in_progress', { title: 'live one' }),
      bead('Z', 'closed', { title: 'finished one' }),
    ]);
    const inProgress = screen.getByRole('region', { name: 'in progress' });
    expect(within(inProgress).getByText('live one')).toBeTruthy();
    const done = screen.getByRole('region', { name: 'done' });
    expect(within(done).getByText('finished one')).toBeTruthy();
  });

  it('calls onSelect when a bead is clicked', () => {
    const { onSelect } = renderBoard([bead('A', 'open', { title: 'pick me' })]);
    fireEvent.click(screen.getByText('pick me'));
    expect(onSelect).toHaveBeenCalledWith('A');
  });

  it('annotates a row with its dependency neighbourhood (full tree lives in the modal)', () => {
    // B needs A: B carries a `needs 1` annotation; A (downstream) carries
    // `blocks 1`. The navigable tree itself moved to BeadDependencies — see
    // BeadDependencies.test — so the row only summarises the counts.
    renderBoard([bead('A', 'closed'), bead('B', 'open', { needs: ['A'] })]);
    expect(screen.getByText(/needs 1/)).toBeTruthy();
    expect(screen.getByText(/blocks 1/)).toBeTruthy();
  });

  it('scrolls the selected bead row into view on initial deep-link selection', () => {
    renderBoard([bead('A', 'open', { title: 'deep linked bead' })], 'A');

    expect(screen.getByTitle('Select A').getAttribute('aria-pressed')).toBe('true');
    expect(scrollIntoView).toHaveBeenCalledWith({
      block: 'center',
      inline: 'nearest',
    });
  });

  it('keeps dependency neighbourhoods row-scoped while the selected row remains clickable', () => {
    const onSelect = vi.fn();
    // A is closed (done column); B needs A so B is ready (ready column). The
    // board row carries only the compact neighbourhood summary; the full
    // navigable dependency tree lives in the bead detail modal.
    renderBoard([bead('A', 'closed'), bead('B', 'open', { needs: ['A'] })], 'B', onSelect);
    const ready = screen.getByRole('region', { name: 'ready' });
    expect(within(ready).getByText(/needs 1/)).toBeTruthy();
    expect(within(ready).queryByTitle('Select A')).toBeNull();
    fireEvent.click(within(ready).getByTitle('Select B'));
    expect(onSelect).toHaveBeenCalledWith('B');
  });

  it('flags an unresolved upstream edge on the row summary', () => {
    renderBoard([bead('B', 'open', { needs: ['GHOST'] })], 'B');
    expect(screen.queryByTitle('Select GHOST')).toBeNull();
    expect(screen.getAllByText('unresolved').length).toBeGreaterThan(0);
  });

  it('keeps the board to at most one maroon mark (the blocked count)', () => {
    const { container } = renderBoard([
      bead('A', 'blocked'),
      bead('B', 'in_progress'),
      bead('C', 'open'),
    ]);
    assertAtMostOneMark(container);
  });

  it('does not carry status in colour alone — every column is a named heading', () => {
    // Greyscale Test: the status of a bead is readable from the column
    // heading it sits under, not from a per-row colour.
    renderBoard([bead('A', 'blocked', { title: 'stuck one' })]);
    const blocked = screen.getByRole('region', { name: 'blocked' });
    expect(within(blocked).getByText('stuck one')).toBeTruthy();
  });
});
