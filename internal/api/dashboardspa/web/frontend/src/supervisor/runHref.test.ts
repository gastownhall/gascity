import { describe, expect, it } from 'vitest';

import type { RunLaneScope } from 'gas-city-dashboard-shared';
import { runDetailHref } from './runHref';

describe('runDetailHref', () => {
  it('appends scope query for an available rig scope (gascity-dashboard-km0w)', () => {
    // A lane whose scope was derived from gc.root_store_ref:'rig:gascity-packs'
    // must deep-link to the correctly-scoped detail fetch, not the default
    // (city) scope that triggers the 12-14s full-store scan + 404.
    const scope: RunLaneScope = {
      status: 'available',
      kind: 'rig',
      ref: 'gascity-packs',
      rootStoreRef: 'rig:gascity-packs',
    };

    expect(runDetailHref('gpk-4fyo6', scope)).toBe(
      '/runs/gpk-4fyo6?scope_kind=rig&scope_ref=gascity-packs',
    );
  });

  it('appends scope query for an available city scope', () => {
    const scope: RunLaneScope = {
      status: 'available',
      kind: 'city',
      ref: 'ds-research',
      rootStoreRef: 'city:ds-research',
    };

    expect(runDetailHref('city-run', scope)).toBe(
      '/runs/city-run?scope_kind=city&scope_ref=ds-research',
    );
  });

  it('omits the scope query when the scope is unavailable', () => {
    const scope: RunLaneScope = {
      status: 'unavailable',
      error: 'run scope metadata unavailable',
    };

    expect(runDetailHref('gpk-4fyo6', scope)).toBe('/runs/gpk-4fyo6');
  });

  it('encodes the run id path segment', () => {
    const scope: RunLaneScope = { status: 'unavailable', error: 'x' };
    expect(runDetailHref('a/b c', scope)).toBe('/runs/a%2Fb%20c');
  });
});
