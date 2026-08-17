import { describe, it, expect } from 'vitest';
import type { FrontendViewDescriptor } from './types';
import { filterEnabledViews, resolveDefaultView } from './resolve';

// PR-C / bead 9yj.5 — view filtering + `/` resolution.
//
// Uses synthetic fixtures (not ALL_VIEWS) so the test does not break when
// the real registry grows. Each fixture covers one rule from the
// resolver's JSDoc.

type Kind = 'core' | 'firstParty';

interface FixtureOpts {
  id: string;
  kind: Kind;
  defaultRoute?: boolean;
  navOrder?: number;
}

function fakeView({ id, kind, defaultRoute, navOrder }: FixtureOpts): FrontendViewDescriptor {
  return {
    id,
    kind,
    path: `/${id}`,
    nav: navOrder !== undefined ? { label: id, order: navOrder } : null,
    // The frontend descriptor narrows element to LazyExoticComponent; the
    // resolver only ever reads .id / .kind / .nav / .defaultRoute, so a
    // structural cast is fine for these unit tests and keeps the fixture
    // from having to materialise a real React.lazy() chunk.
    element: {} as FrontendViewDescriptor['element'],
    ...(defaultRoute !== undefined ? { defaultRoute } : {}),
  };
}

const core = fakeView({ id: 'health', kind: 'core', navOrder: 60 });
const maintainer = fakeView({
  id: 'maintainer',
  kind: 'firstParty',
  navOrder: 80,
});
const ambient = fakeView({
  id: 'ambient',
  kind: 'firstParty',
  navOrder: 10,
  defaultRoute: true,
});
const secondary = fakeView({
  id: 'secondary',
  kind: 'firstParty',
  navOrder: 5,
  defaultRoute: true,
});

const descriptorWithLegacyPaths = {
  id: 'legacy',
  kind: 'firstParty',
  path: '/legacy',
  nav: null,
  element: {} as FrontendViewDescriptor['element'],
  // @ts-expect-error legacyPaths is intentionally not part of FrontendViewDescriptor.
  legacyPaths: ['/old'],
} satisfies FrontendViewDescriptor;
void descriptorWithLegacyPaths;

describe('filterEnabledViews', () => {
  it('keeps only core views when enabledModules is null (unset/not-yet-loaded → core-only, PR-D)', () => {
    const result = filterEnabledViews([core, maintainer], null);
    expect(result.map((v) => v.id)).toEqual(['health']);
  });

  it('keeps core views even when enabledModules is the empty set', () => {
    const result = filterEnabledViews([core, maintainer], []);
    expect(result.map((v) => v.id)).toEqual(['health']);
  });

  it('keeps firstParty views named in enabledModules', () => {
    const result = filterEnabledViews([core, maintainer, ambient], ['maintainer']);
    expect(result.map((v) => v.id).sort()).toEqual(['health', 'maintainer']);
  });

  it('drops firstParty views absent from enabledModules', () => {
    const result = filterEnabledViews([core, maintainer, ambient], []);
    expect(result.map((v) => v.id)).toEqual(['health']);
  });
});

describe('resolveDefaultView', () => {
  it('returns view: null when no descriptor is flagged and no env override is given', () => {
    const result = resolveDefaultView([core, maintainer], null);
    expect(result).toEqual({ view: null, source: 'fallback', warnings: [] });
  });

  it('returns the single defaultRoute view via the descriptor path', () => {
    const result = resolveDefaultView([core, ambient], null);
    expect(result.view?.id).toBe('ambient');
    expect(result.source).toBe('descriptor');
    expect(result.warnings).toEqual([]);
  });

  it('honours DEFAULT_VIEW env override when it names an enabled view', () => {
    const result = resolveDefaultView([core, maintainer, ambient], 'maintainer');
    expect(result.view?.id).toBe('maintainer');
    expect(result.source).toBe('env');
    expect(result.warnings).toEqual([]);
  });

  it('warns + falls through when DEFAULT_VIEW names an UNKNOWN id', () => {
    const result = resolveDefaultView([core, ambient], 'nope');
    expect(result.view?.id).toBe('ambient');
    expect(result.source).toBe('descriptor');
    expect(result.warnings).toHaveLength(1);
    expect(result.warnings[0]).toContain('DEFAULT_VIEW="nope"');
  });

  it('warns + falls through when DEFAULT_VIEW names a DISABLED module', () => {
    // 'maintainer' is in the registry conceptually but ABSENT from
    // enabledViews — exactly the MODULES_ENABLED=health + DEFAULT_VIEW=maintainer
    // combination called out in the bead's acceptance criteria.
    const result = resolveDefaultView([core], 'maintainer');
    expect(result.view).toBeNull();
    expect(result.source).toBe('fallback');
    expect(result.warnings).toHaveLength(1);
    expect(result.warnings[0]).toContain('DEFAULT_VIEW="maintainer"');
  });

  it('warns + picks lowest nav.order when multiple views flag defaultRoute: true', () => {
    const result = resolveDefaultView([secondary, ambient], null);
    // secondary navOrder=5, ambient navOrder=10 — secondary wins.
    expect(result.view?.id).toBe('secondary');
    expect(result.source).toBe('descriptor');
    expect(result.warnings).toHaveLength(1);
    expect(result.warnings[0]).toContain('multiple views declare defaultRoute');
  });

  it('does NOT set redirectTo for descriptor-flag resolution', () => {
    const result = resolveDefaultView([core, ambient], null);
    expect(result.redirectTo).toBeUndefined();
    expect(result.view?.id).toBe('ambient');
  });
});
