import { describe, expect, it } from 'vitest';
import { composeAttention } from './compose';
import type { AttentionDomain, AttentionItem, AttentionModel, AttentionSeverity } from './compose';
import { prefixedAttentionSeverity, resourceAttentionSeverity } from './routeHighlight';

function item(id: string, severity: AttentionSeverity, domain: AttentionDomain): AttentionItem {
  return { id, domain, severity, title: id };
}

function modelWith(items: readonly AttentionItem[]): AttentionModel {
  // composeAttention groups by item.domain, so a single contributor carries
  // items across whatever domains the test seeds.
  return composeAttention([{ id: 'test', domain: 'runs', getItems: () => items }]);
}

describe('resourceAttentionSeverity', () => {
  it('returns null when only unavailable items match the resource', () => {
    const model = modelWith([item('runs:lane1', 'unavailable', 'runs')]);
    expect(resourceAttentionSeverity(model, 'runs', 'lane1')).toBeNull();
  });

  it('returns watch (not null) when a watch item is mixed with unavailable', () => {
    const model = modelWith([
      item('runs:lane1', 'watch', 'runs'),
      item('runs:lane1:sub', 'unavailable', 'runs'),
    ]);
    expect(resourceAttentionSeverity(model, 'runs', 'lane1')).toBe('watch');
  });

  it('returns attention when an attention item is mixed with unavailable', () => {
    const model = modelWith([
      item('runs:lane1:sub', 'attention', 'runs'),
      item('runs:lane1', 'unavailable', 'runs'),
    ]);
    expect(resourceAttentionSeverity(model, 'runs', 'lane1')).toBe('attention');
  });
});

describe('prefixedAttentionSeverity', () => {
  const prefixes = ['health:tool:'] as const;

  it('returns null when only unavailable items match the prefix', () => {
    const model = modelWith([item('health:tool:dolt', 'unavailable', 'health')]);
    expect(prefixedAttentionSeverity(model, 'health', prefixes)).toBeNull();
  });

  it('returns watch (not null) when a watch item is mixed with unavailable', () => {
    const model = modelWith([
      item('health:tool:dolt', 'watch', 'health'),
      item('health:tool:gc', 'unavailable', 'health'),
    ]);
    expect(prefixedAttentionSeverity(model, 'health', prefixes)).toBe('watch');
  });

  it('returns attention when an attention item is mixed with unavailable', () => {
    const model = modelWith([
      item('health:tool:dolt', 'attention', 'health'),
      item('health:tool:gc', 'unavailable', 'health'),
    ]);
    expect(prefixedAttentionSeverity(model, 'health', prefixes)).toBe('attention');
  });
});
