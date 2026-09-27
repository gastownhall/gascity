import { describe, expect, it } from 'vitest';
import { isAllowedPreviewUrl, resolvePreview } from './workbenchPreview';
import type { SupervisorBead } from '../supervisor/beadReads';

function bead(metadata: Record<string, string> = {}): SupervisorBead {
  return { id: 'gascity-1', metadata } as unknown as SupervisorBead;
}

describe('resolvePreview', () => {
  it('reports missing when the Bead advertises no preview', () => {
    expect(resolvePreview(bead()).state).toBe('missing');
  });

  it('resolves an allowlisted loopback preview', () => {
    const got = resolvePreview(bead({ 'gc.preview_url': 'http://localhost:3100/preview' }));
    expect(got.state).toBe('ok');
    expect(got.host).toBe('localhost');
  });

  it('blocks a non-allowlisted host and never returns a URL', () => {
    const got = resolvePreview(bead({ 'gc.preview_url': 'https://evil.example.com/x' }));
    expect(got.state).toBe('blocked');
    expect(got.url).toBeNull();
  });

  it('reports unavailable for a malformed URL', () => {
    expect(resolvePreview(bead({ 'gc.preview_url': 'not a url' })).state).toBe('unavailable');
  });

  it('rejects non-http(s) schemes', () => {
    expect(isAllowedPreviewUrl('javascript:alert(1)')).toBe(false);
    expect(isAllowedPreviewUrl('http://127.0.0.1:8080/')).toBe(true);
  });
});
