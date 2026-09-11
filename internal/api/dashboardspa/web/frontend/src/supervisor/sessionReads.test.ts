import { describe, expect, it } from 'vitest';
import type { SessionTranscriptGetResponse } from 'gas-city-dashboard-shared/gc-supervisor';
import { structuredTranscriptOrNull } from './sessionReads';

describe('structuredTranscriptOrNull', () => {
  it('returns null only for a non-structured response', () => {
    const conversation = {
      id: 'session-1',
      template: 'worker',
      provider: 'claude',
      format: 'conversation',
      turns: [],
    } as SessionTranscriptGetResponse;

    expect(structuredTranscriptOrNull(conversation)).toBeNull();
  });

  it('rejects a malformed structured response instead of silently falling back', () => {
    const malformed = {
      id: 'session-1',
      template: 'worker',
      provider: 'claude',
      format: 'structured',
      schema_version: 'session.structured.v1',
      operation: 'snapshot',
      history: {},
      structured_messages: [],
    } as unknown as SessionTranscriptGetResponse;

    expect(() => structuredTranscriptOrNull(malformed)).toThrow(
      'Malformed structured transcript response.',
    );
  });
});
