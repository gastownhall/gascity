import { cleanup, render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it } from 'vitest';
import { AttentionProvider } from './context';
import { AttentionSummaryPanel } from './AttentionSummaryPanel';
import type { AttentionContributor, AttentionItem } from './compose';

describe('AttentionSummaryPanel', () => {
  afterEach(() => {
    cleanup();
  });

  it('renders top attention items and grouped overflow from the shared model', () => {
    render(
      <MemoryRouter
        basename="/city/test-city"
        initialEntries={['/city/test-city/']}
        future={{ v7_relativeSplatPath: true, v7_startTransition: true }}
      >
        <AttentionProvider
          contributors={[
            contributor('runs', [
              item('run-1', 'runs', 'attention', { title: 'Run blocked', actionable: true }),
              item('run-2', 'runs', 'watch', { title: 'Run stale' }),
            ]),
            contributor('mail', [
              item('mail-1', 'mail', 'watch', { title: 'Mayor unread' }),
              item('mail-2', 'mail', 'watch', { title: 'Clerk unread' }),
            ]),
          ]}
          topLimit={2}
        >
          <AttentionSummaryPanel />
        </AttentionProvider>
      </MemoryRouter>,
    );

    expect(screen.getByRole('heading', { name: 'Attention' })).toBeTruthy();
    expect(screen.getByRole('link', { name: 'Run blocked' }).getAttribute('href')).toBe(
      '/city/test-city/runs',
    );
    expect(screen.getByText('Run stale')).toBeTruthy();
    expect(screen.getByRole('link', { name: '2 more in Mail' }).getAttribute('href')).toBe(
      '/city/test-city/mail',
    );
    expect(screen.queryByText('Mayor unread')).toBeNull();
  });

  it('renders unavailable items with a muted tone, never the warn/accent badge colors', () => {
    render(
      <MemoryRouter
        basename="/city/test-city"
        initialEntries={['/city/test-city/']}
        future={{ v7_relativeSplatPath: true, v7_startTransition: true }}
      >
        <AttentionProvider
          contributors={[
            contributor('runs', [
              item('runs:feed-partial', 'runs', 'unavailable', {
                title: 'Formula run feed incomplete',
                provenance: 'stale',
              }),
            ]),
          ]}
        >
          <AttentionSummaryPanel />
        </AttentionProvider>
      </MemoryRouter>,
    );

    expect(screen.getByText('Formula run feed incomplete')).toBeTruthy();
    const tag = screen.getByText('Runs');
    expect(tag.className).toContain('text-fg-muted');
    expect(tag.className).not.toContain('text-warn');
    expect(tag.className).not.toContain('text-accent');
  });

  it('renders nothing when there are no attention facts', () => {
    const { container } = render(
      <AttentionProvider contributors={[]}>
        <AttentionSummaryPanel />
      </AttentionProvider>,
    );

    expect(container.firstChild).toBeNull();
  });
});

function contributor(
  domain: AttentionItem['domain'],
  items: readonly AttentionItem[],
): AttentionContributor {
  return {
    id: `${domain}-test`,
    domain,
    getItems: () => items,
  };
}

function item(
  id: string,
  domain: AttentionItem['domain'],
  severity: AttentionItem['severity'],
  overrides: Partial<AttentionItem> = {},
): AttentionItem {
  return {
    id,
    domain,
    severity,
    title: id,
    href: `/${domain}`,
    current: true,
    actionable: false,
    ...overrides,
  };
}
