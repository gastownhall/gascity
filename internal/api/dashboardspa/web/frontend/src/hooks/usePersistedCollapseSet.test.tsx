import { act, cleanup, renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it, vi, type Mock } from 'vitest';
import { reportClientError } from '../lib/clientErrorReporting';
import { usePersistedCollapseSet } from './usePersistedCollapseSet';

vi.mock('../lib/clientErrorReporting', () => ({
  reportClientError: vi.fn(() => Promise.resolve({ status: 'reported' })),
}));

const mockReportClientError = reportClientError as Mock;
const KEY = 'test:collapsed';
const COMPONENT = 'TestComponent';

describe('usePersistedCollapseSet', () => {
  afterEach(() => {
    cleanup();
    window.localStorage.clear();
    window.sessionStorage.clear();
    mockReportClientError.mockClear();
  });

  it('starts empty when no persisted value exists', () => {
    const { result } = renderHook(() =>
      usePersistedCollapseSet({ key: KEY, component: COMPONENT }),
    );

    expect(result.current.isCollapsed('tier:breaking')).toBe(false);
    expect(window.localStorage.getItem(KEY)).toBeNull();
  });

  it('initializes from a persisted JSON string array', () => {
    window.localStorage.setItem(KEY, JSON.stringify(['tier:breaking', 'cluster:core']));

    const { result } = renderHook(() =>
      usePersistedCollapseSet({ key: KEY, component: COMPONENT }),
    );

    expect(result.current.isCollapsed('tier:breaking')).toBe(true);
    expect(result.current.isCollapsed('cluster:core')).toBe(true);
    expect(result.current.isCollapsed('cluster:docs')).toBe(false);
  });

  it('toggles ids and persists the updated set', () => {
    const { result } = renderHook(() =>
      usePersistedCollapseSet({ key: KEY, component: COMPONENT }),
    );

    act(() => result.current.toggle('tier:breaking'));

    expect(result.current.isCollapsed('tier:breaking')).toBe(true);
    expect(JSON.parse(window.localStorage.getItem(KEY) ?? 'null')).toEqual(['tier:breaking']);

    act(() => result.current.toggle('tier:breaking'));

    expect(result.current.isCollapsed('tier:breaking')).toBe(false);
    expect(JSON.parse(window.localStorage.getItem(KEY) ?? 'null')).toEqual([]);
  });

  it('persists an exact replacement set', () => {
    const { result } = renderHook(() =>
      usePersistedCollapseSet({ key: KEY, component: COMPONENT }),
    );

    act(() => result.current.setExact(['tier:breaking', 'cluster:docs']));

    expect(result.current.isCollapsed('tier:breaking')).toBe(true);
    expect(result.current.isCollapsed('cluster:docs')).toBe(true);
    expect(JSON.parse(window.localStorage.getItem(KEY) ?? 'null')).toEqual([
      'tier:breaking',
      'cluster:docs',
    ]);
  });

  it('reports corrupt persisted state and falls back to empty', () => {
    window.localStorage.setItem(KEY, JSON.stringify(['tier:breaking', 42]));

    const { result } = renderHook(() =>
      usePersistedCollapseSet({ key: KEY, component: COMPONENT }),
    );

    expect(result.current.isCollapsed('tier:breaking')).toBe(false);
    expect(mockReportClientError).toHaveBeenCalledTimes(1);
    expect(mockReportClientError).toHaveBeenCalledWith({
      component: COMPONENT,
      operation: 'localStorage.parse',
      message: `${KEY}: expected an array of strings`,
    });
  });
});
