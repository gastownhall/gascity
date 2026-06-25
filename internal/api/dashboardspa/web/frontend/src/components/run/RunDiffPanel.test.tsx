import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import type { RunDiffResponse } from 'gas-city-dashboard-shared';
import { RunDiffPanel } from './RunDiffPanel';

afterEach(() => cleanup());

describe('RunDiffPanel', () => {
  it('shows quiet skipped states when the execution folder is unknown or not git', () => {
    const { container, rerender } = render(<RunDiffPanel diff={diffFor('path_unknown')} />);

    // A missing work_dir is a calm absence, not a failure: explain why there
    // is no diff and never raise the maroon accent for it.
    expect(screen.getByText(/no diff available for this run/i)).toBeTruthy();
    expect(screen.getByText(/did not record a work_dir/i)).toBeTruthy();
    expect(container.querySelectorAll('.text-accent').length).toBe(0);

    rerender(<RunDiffPanel diff={diffFor('not_git')} />);
    expect(screen.getByText(/not a git work tree/i)).toBeTruthy();
  });

  it('renders a grouped OSS unified patch view for tracked and untracked files', () => {
    const { container } = render(<RunDiffPanel diff={diffWithPatch()} />);

    expect(screen.getByRole('heading', { name: 'Local Changes' })).toBeTruthy();
    expect(screen.getByText(/compared with origin\/main/i)).toBeTruthy();
    expect(screen.getByText('src/run.ts')).toBeTruthy();
    expect(screen.getByText('docs/plan.md')).toBeTruthy();
    expect(screen.queryByText(/^\?\? docs\/plan\.md/)).toBeNull();
    expect(screen.getByText(/diff truncated/i)).toBeTruthy();
    const gutterSigns = [...container.querySelectorAll('.diff-gutter-sign')].map(
      (node) => node.textContent ?? '',
    );
    expect(gutterSigns).toContain('+');
    expect(gutterSigns).toContain('-');
    expect(container.querySelector('.diff-code-insert')?.textContent).toContain('new session');
    expect(container.querySelector('.diff-code-delete')?.textContent).toContain('old session');
  });

  it('never colors body-type diff lines with the maroon accent (One Mark Rule + Greyscale Test)', () => {
    const { container } = render(<RunDiffPanel diff={diffWithPatch()} />);

    expect(container.querySelectorAll('.text-accent').length).toBe(0);
  });
});

function diffFor(kind: 'path_unknown' | 'not_git'): RunDiffResponse {
  return {
    kind,
    rootPath: { kind: 'unavailable', reason: kind },
    comparison: { kind: 'unavailable', reason: kind },
    status: [],
    changedFiles: [],
    patch: '',
    truncated: false,
  };
}

function diffWithPatch(): RunDiffResponse {
  return {
    kind: 'ok',
    rootPath: { kind: 'known', path: '/tmp/rig' },
    comparison: {
      kind: 'upstream',
      ref: 'origin/main',
      mergeBase: '4c2ebf903c64a368a1043878f6e8a0ee3666633f',
    },
    status: [' M src/run.ts', '?? docs/plan.md'],
    changedFiles: [
      { path: 'src/run.ts', status: 'M', kind: 'code' },
      { path: 'docs/plan.md', status: '??', kind: 'docs' },
    ],
    patch: [
      'diff --git a/src/run.ts b/src/run.ts',
      'index 3a4e79a..b6c9d02 100644',
      '--- a/src/run.ts',
      '+++ b/src/run.ts',
      '@@ -1 +1 @@',
      '-old session',
      '+new session',
      '',
      'diff --git a/docs/plan.md b/docs/plan.md',
      'new file mode 100644',
      'index 0000000..b6c9d02',
      '--- /dev/null',
      '+++ b/docs/plan.md',
      '@@ -0,0 +1 @@',
      '+plan output',
    ].join('\n'),
    truncated: true,
  };
}
