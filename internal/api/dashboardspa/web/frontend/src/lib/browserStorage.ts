import { errorMessage } from 'gas-city-dashboard-shared';
import { reportClientError } from './clientErrorReporting';

export type BrowserStorageArea = 'localStorage' | 'sessionStorage';

export type BrowserStorageReadResult =
  | { status: 'found'; value: string }
  | { status: 'missing' }
  | { status: 'unavailable'; error: string };

export type BrowserStorageWriteResult =
  | { status: 'stored' }
  | { status: 'unavailable'; error: string };

export function readBrowserStorage(
  area: BrowserStorageArea,
  key: string,
  component: string,
): BrowserStorageReadResult {
  try {
    const value = storage(area).getItem(key);
    if (value === null) return { status: 'missing' };
    return { status: 'found', value };
  } catch (err) {
    return storageUnavailable(area, 'getItem', key, component, err);
  }
}

export function writeBrowserStorage(
  area: BrowserStorageArea,
  key: string,
  value: string,
  component: string,
): BrowserStorageWriteResult {
  try {
    storage(area).setItem(key, value);
    return { status: 'stored' };
  } catch (err) {
    return storageUnavailable(area, 'setItem', key, component, err);
  }
}

export function removeBrowserStorage(
  area: BrowserStorageArea,
  key: string,
  component: string,
): BrowserStorageWriteResult {
  try {
    storage(area).removeItem(key);
    return { status: 'stored' };
  } catch (err) {
    return storageUnavailable(area, 'removeItem', key, component, err);
  }
}

function storage(area: BrowserStorageArea): Storage {
  return area === 'localStorage' ? window.localStorage : window.sessionStorage;
}

function storageUnavailable(
  area: BrowserStorageArea,
  operation: 'getItem' | 'setItem' | 'removeItem',
  key: string,
  component: string,
  err: unknown,
): { status: 'unavailable'; error: string } {
  const error = errorMessage(err);
  void reportClientError({
    component,
    operation: `${area}.${operation}`,
    message: `${key}: ${error}`,
  });
  return { status: 'unavailable', error };
}
