import { afterAll, afterEach, beforeAll, beforeEach, vi } from 'vitest';
import { setActiveCity } from '../api/cityBase';

// gascity-dashboard-ucc: city-scoped `api.*` calls and EventSource URLs are
// built off the module-level active city (set by the router from the URL
// segment in production). Tests render components in isolation without the
// router, so seed a deterministic active city here — every city-scoped
// request then resolves to `/api/city/test-city/*`, which fetch mocks match.
export const TEST_CITY = 'test-city';

let warnSpy: ReturnType<typeof vi.spyOn>;
let infoSpy: ReturnType<typeof vi.spyOn>;
const processWarnings: string[] = [];

beforeAll(() => {
  process.on('warning', trackProcessWarning);
  installDeterministicStorage();
});

beforeEach(() => {
  setActiveCity(TEST_CITY);
  warnSpy = vi.spyOn(console, 'warn').mockImplementation((...args: unknown[]) => {
    throw new Error(`Unexpected console.warn: ${formatConsoleArgs(args)}`);
  });
  infoSpy = vi.spyOn(console, 'info').mockImplementation((...args: unknown[]) => {
    throw new Error(`Unexpected console.info: ${formatConsoleArgs(args)}`);
  });
});

afterEach(() => {
  warnSpy.mockRestore();
  infoSpy.mockRestore();

  if (processWarnings.length > 0) {
    const warnings = processWarnings.splice(0).join('\n');
    throw new Error(`Unexpected process warning:\n${warnings}`);
  }
});

afterAll(() => {
  process.off('warning', trackProcessWarning);
});

function formatConsoleArgs(args: unknown[]): string {
  return args.map((arg) => (typeof arg === 'string' ? arg : JSON.stringify(arg))).join(' ');
}

// Node 25 ships an experimental localStorage gated by --localstorage-file; the
// vitest jsdom environment passes that flag without a path, so Node emits a
// benign runtime warning the moment a worker touches localStorage. It is
// infrastructure noise, not a signal from application or test code, so it must
// not fail the suite — the guard exists to catch our own warnings (deprecations,
// unhandled rejections), not the runner's flag bookkeeping.
function isInfrastructureWarning(warning: Error): boolean {
  return warning.message.includes('--localstorage-file');
}

function trackProcessWarning(warning: Error): void {
  if (isInfrastructureWarning(warning)) return;
  processWarnings.push(`${warning.name}: ${warning.message}`);
}

function installDeterministicStorage(): void {
  if (typeof window === 'undefined') return;

  try {
    if (typeof window.localStorage?.clear === 'function') {
      return;
    }
  } catch {
    // Opaque jsdom origins throw here; tests should still get storage.
  }

  const storage = new MemoryStorage();
  Object.defineProperty(window, 'localStorage', {
    configurable: true,
    value: storage,
  });
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    value: storage,
  });
}

class MemoryStorage implements Storage {
  readonly #items = new Map<string, string>();

  get length(): number {
    return this.#items.size;
  }

  clear(): void {
    this.#items.clear();
  }

  getItem(key: string): string | null {
    return this.#items.get(key) ?? null;
  }

  key(index: number): string | null {
    return Array.from(this.#items.keys())[index] ?? null;
  }

  removeItem(key: string): void {
    this.#items.delete(key);
  }

  setItem(key: string, value: string): void {
    this.#items.set(key, value);
  }
}
