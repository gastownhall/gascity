import { createClient, type Client as GeneratedSupervisorClient } from '@hey-api/client-fetch';
import {
  createBead as postSupervisorBead,
  getHealth,
  getV0Cities,
  getV0CityByCityNameAgents,
  getV0CityByCityNameBeadById,
  getV0CityByCityNameBeads,
  getV0CityByCityNameEvents,
  getV0CityByCityNameFormulasByName,
  getV0CityByCityNameFormulasFeed,
  getV0CityByCityNameHealth,
  getV0CityByCityNameMail,
  getV0CityByCityNameMailThreadById,
  getV0CityByCityNameRigs,
  getV0CityByCityNameSessionByIdPending,
  getV0CityByCityNameSessionByIdTranscript,
  getV0CityByCityNameSessions,
  getV0CityByCityNameStatus,
  getV0CityByCityNameWorkflowByWorkflowId,
  patchV0CityByCityNameBeadById,
  postV0CityByCityNameBeadByIdClose,
  postV0CityByCityNameSling,
  postV0CityByCityNameMailByIdArchive,
  postV0CityByCityNameMailByIdMarkUnread,
  postV0CityByCityNameMailByIdRead,
  replyMail as postSupervisorMailReply,
  respondSession as postSupervisorSessionRespond,
  sendMail as postSupervisorMail,
} from 'gas-city-dashboard-shared/gc-supervisor';
import type {
  Bead,
  BeadCreateInputBody,
  BeadUpdateBody,
  FormulaFeedBody,
  GetV0CityByCityNameBeadsData,
  GetV0CityByCityNameEventsData,
  GetV0CityByCityNameFormulasFeedData,
  GetV0CityByCityNameFormulasByNameData,
  GetHealthResponse,
  GetV0CityByCityNameHealthResponse,
  GetV0CityByCityNameMailData,
  GetV0CityByCityNameStatusResponse,
  GetV0CityByCityNameWorkflowByWorkflowIdData,
  ListBodyBead,
  ListBodyAgentResponse,
  ListBodyRigResponse,
  ListBodyWireEvent,
  MailReplyInputBody,
  MailSendInputBody,
  FormulaDetailResponse,
  MailListBody,
  Message,
  ListBodySessionResponse,
  OkResponseBody,
  PostV0CityByCityNameMailByIdArchiveData,
  PostV0CityByCityNameMailByIdMarkUnreadData,
  PostV0CityByCityNameMailByIdReadData,
  ReplyMailData,
  RespondSessionResponse,
  SessionTranscriptGetResponse,
  SessionPendingResponse,
  SessionRespondInputBody,
  SlingInputBody,
  SlingResponse,
  SupervisorCitiesOutputBody,
  WorkflowSnapshotResponse,
} from 'gas-city-dashboard-shared/gc-supervisor';
import { SupervisorApiError, unwrapSupervisorResult, type SupervisorResult } from './errors';
import {
  SUPERVISOR_PROXY_BASE_URL,
  resolveClientBaseUrl,
  resolveSupervisorBaseUrl,
  supervisorUrl,
} from './url';

export const SUPERVISOR_REQUEST_TIMEOUT_MS = 60_000;
export const GC_MUTATION_HEADERS = {
  'X-GC-Request': 'dashboard',
} as const;

export { SupervisorApiError, SUPERVISOR_PROXY_BASE_URL };

export interface SupervisorApi {
  readonly baseUrl: string;
  health(): Promise<GetHealthResponse>;
  cityHealth(cityName: string): Promise<GetV0CityByCityNameHealthResponse>;
  cityStatus(cityName: string): Promise<GetV0CityByCityNameStatusResponse>;
  listCities(): Promise<SupervisorCitiesOutputBody>;
  listAgents(cityName: string): Promise<ListBodyAgentResponse>;
  listRigs(cityName: string): Promise<ListBodyRigResponse>;
  listBeads(
    cityName: string,
    query?: NonNullable<GetV0CityByCityNameBeadsData['query']>,
  ): Promise<ListBodyBead>;
  listEvents(
    cityName: string,
    query?: NonNullable<GetV0CityByCityNameEventsData['query']>,
  ): Promise<ListBodyWireEvent>;
  getBead(cityName: string, id: string): Promise<Bead>;
  createBead(cityName: string, body: BeadCreateInputBody): Promise<Bead>;
  updateBead(cityName: string, id: string, body: BeadUpdateBody): Promise<OkResponseBody>;
  closeBead(cityName: string, id: string): Promise<OkResponseBody>;
  sling(cityName: string, body: SlingInputBody): Promise<SlingResponse>;
  listMail(
    cityName: string,
    query?: NonNullable<GetV0CityByCityNameMailData['query']>,
  ): Promise<MailListBody>;
  formulaFeed(
    cityName: string,
    query?: NonNullable<GetV0CityByCityNameFormulasFeedData['query']>,
  ): Promise<FormulaFeedBody>;
  sendMail(cityName: string, body: MailSendInputBody): Promise<Message>;
  mailThread(cityName: string, threadId: string): Promise<MailListBody>;
  markMailRead(
    cityName: string,
    id: string,
    query?: NonNullable<PostV0CityByCityNameMailByIdReadData['query']>,
  ): Promise<OkResponseBody>;
  markMailUnread(
    cityName: string,
    id: string,
    query?: NonNullable<PostV0CityByCityNameMailByIdMarkUnreadData['query']>,
  ): Promise<OkResponseBody>;
  archiveMail(
    cityName: string,
    id: string,
    query?: NonNullable<PostV0CityByCityNameMailByIdArchiveData['query']>,
  ): Promise<OkResponseBody>;
  replyMail(
    cityName: string,
    id: string,
    body: MailReplyInputBody,
    query?: NonNullable<ReplyMailData['query']>,
  ): Promise<Message>;
  cityEventStreamUrl(cityName: string, afterSeq?: string): string;
  sessionStreamUrl(cityName: string, sessionId: string, after?: string): string;
  listSessions(cityName: string): Promise<ListBodySessionResponse>;
  sessionPending(cityName: string, sessionId: string): Promise<SessionPendingResponse>;
  respondSession(
    cityName: string,
    sessionId: string,
    body: SessionRespondInputBody,
  ): Promise<RespondSessionResponse>;
  sessionTranscript(cityName: string, sessionId: string): Promise<SessionTranscriptGetResponse>;
  workflowRun(
    cityName: string,
    workflowId: string,
    query?: NonNullable<GetV0CityByCityNameWorkflowByWorkflowIdData['query']>,
  ): Promise<WorkflowSnapshotResponse>;
  formulaDetail(
    cityName: string,
    name: string,
    query: GetV0CityByCityNameFormulasByNameData['query'],
  ): Promise<FormulaDetailResponse>;
  mutationHeaders(): Record<keyof typeof GC_MUTATION_HEADERS, string>;
}

export interface CreateSupervisorApiOptions {
  baseUrl?: string;
  fetch?: typeof fetch;
  client?: GeneratedSupervisorClient;
  timeoutMs?: number;
}

let testSupervisorApi: SupervisorApi | null = null;
let defaultSupervisorApi: SupervisorApi | null = null;
const requestBudgetSupervisorApis = new Map<number, SupervisorApi>();

export function createSupervisorApi(options: CreateSupervisorApiOptions = {}): SupervisorApi {
  const baseUrl = options.baseUrl ?? resolveSupervisorBaseUrl();
  const clientBaseUrl = resolveClientBaseUrl(baseUrl);
  const clientOptions = {
    baseUrl: clientBaseUrl,
    headers: { Accept: 'application/json' },
    responseStyle: 'fields' as const,
    throwOnError: false,
  };
  const client =
    options.client ??
    createClient({
      ...clientOptions,
      fetch: withSupervisorTimeout(
        options.fetch ?? globalThis.fetch,
        supervisorTimeoutMs(options.timeoutMs),
      ),
    });

  return {
    baseUrl,
    health() {
      return unwrapSupervisorResult<GetHealthResponse>(
        getHealth({ client }) as Promise<SupervisorResult<GetHealthResponse>>,
        'gc supervisor health response was empty',
      );
    },
    cityHealth(cityName) {
      return unwrapSupervisorResult<GetV0CityByCityNameHealthResponse>(
        getV0CityByCityNameHealth({
          client,
          path: { cityName },
        }) as Promise<SupervisorResult<GetV0CityByCityNameHealthResponse>>,
        'gc supervisor city health response was empty',
      );
    },
    cityStatus(cityName) {
      return unwrapSupervisorResult<GetV0CityByCityNameStatusResponse>(
        getV0CityByCityNameStatus({
          client,
          path: { cityName },
        }) as Promise<SupervisorResult<GetV0CityByCityNameStatusResponse>>,
        'gc supervisor status response was empty',
      );
    },
    listCities() {
      return unwrapSupervisorResult<SupervisorCitiesOutputBody>(
        getV0Cities({ client }) as Promise<SupervisorResult<SupervisorCitiesOutputBody>>,
        'gc supervisor cities response was empty',
      );
    },
    listAgents(cityName) {
      return unwrapSupervisorResult<ListBodyAgentResponse>(
        getV0CityByCityNameAgents({
          client,
          path: { cityName },
        }) as Promise<SupervisorResult<ListBodyAgentResponse>>,
        'gc supervisor agents response was empty',
      );
    },
    listRigs(cityName) {
      return unwrapSupervisorResult<ListBodyRigResponse>(
        getV0CityByCityNameRigs({
          client,
          path: { cityName },
        }) as Promise<SupervisorResult<ListBodyRigResponse>>,
        'gc supervisor rigs response was empty',
      );
    },
    listBeads(cityName, query) {
      return unwrapSupervisorResult<ListBodyBead>(
        getV0CityByCityNameBeads({
          client,
          path: { cityName },
          ...(query === undefined ? {} : { query }),
        }) as Promise<SupervisorResult<ListBodyBead>>,
        'gc supervisor beads response was empty',
      );
    },
    listEvents(cityName, query) {
      return unwrapSupervisorResult<ListBodyWireEvent>(
        getV0CityByCityNameEvents({
          client,
          path: { cityName },
          ...(query === undefined ? {} : { query }),
        }) as Promise<SupervisorResult<ListBodyWireEvent>>,
        'gc supervisor events response was empty',
      );
    },
    getBead(cityName, id) {
      return unwrapSupervisorResult<Bead>(
        getV0CityByCityNameBeadById({
          client,
          path: { cityName, id },
        }) as Promise<SupervisorResult<Bead>>,
        'gc supervisor bead response was empty',
      );
    },
    createBead(cityName, body) {
      return unwrapSupervisorResult<Bead>(
        postSupervisorBead({
          client,
          path: { cityName },
          headers: GC_MUTATION_HEADERS,
          body,
        }) as Promise<SupervisorResult<Bead>>,
        'gc supervisor bead create response was empty',
      );
    },
    updateBead(cityName, id, body) {
      return unwrapSupervisorResult<OkResponseBody>(
        patchV0CityByCityNameBeadById({
          client,
          path: { cityName, id },
          headers: GC_MUTATION_HEADERS,
          body,
        }) as Promise<SupervisorResult<OkResponseBody>>,
        'gc supervisor bead update response was empty',
      );
    },
    closeBead(cityName, id) {
      return unwrapSupervisorResult<OkResponseBody>(
        postV0CityByCityNameBeadByIdClose({
          client,
          path: { cityName, id },
          headers: GC_MUTATION_HEADERS,
        }) as Promise<SupervisorResult<OkResponseBody>>,
        'gc supervisor bead close response was empty',
      );
    },
    sling(cityName, body) {
      return unwrapSupervisorResult<SlingResponse>(
        postV0CityByCityNameSling({
          client,
          path: { cityName },
          headers: GC_MUTATION_HEADERS,
          body,
        }) as Promise<SupervisorResult<SlingResponse>>,
        'gc supervisor sling response was empty',
      );
    },
    listMail(cityName, query) {
      return unwrapSupervisorResult<MailListBody>(
        getV0CityByCityNameMail({
          client,
          path: { cityName },
          ...(query === undefined ? {} : { query }),
        }) as Promise<SupervisorResult<MailListBody>>,
        'gc supervisor mail response was empty',
      );
    },
    formulaFeed(cityName, query) {
      return unwrapSupervisorResult<FormulaFeedBody>(
        getV0CityByCityNameFormulasFeed({
          client,
          path: { cityName },
          ...(query === undefined ? {} : { query }),
        }) as Promise<SupervisorResult<FormulaFeedBody>>,
        'gc supervisor formula feed response was empty',
      );
    },
    sendMail(cityName, body) {
      return unwrapSupervisorResult<Message>(
        postSupervisorMail({
          client,
          path: { cityName },
          headers: GC_MUTATION_HEADERS,
          body,
        }) as Promise<SupervisorResult<Message>>,
        'gc supervisor mail send response was empty',
      );
    },
    mailThread(cityName, threadId) {
      return unwrapSupervisorResult<MailListBody>(
        getV0CityByCityNameMailThreadById({
          client,
          path: { cityName, id: threadId },
        }) as Promise<SupervisorResult<MailListBody>>,
        'gc supervisor mail thread response was empty',
      );
    },
    markMailRead(cityName, id, query) {
      return unwrapSupervisorResult<OkResponseBody>(
        postV0CityByCityNameMailByIdRead({
          client,
          path: { cityName, id },
          headers: GC_MUTATION_HEADERS,
          ...(query === undefined ? {} : { query }),
        }) as Promise<SupervisorResult<OkResponseBody>>,
        'gc supervisor mail mark-read response was empty',
      );
    },
    markMailUnread(cityName, id, query) {
      return unwrapSupervisorResult<OkResponseBody>(
        postV0CityByCityNameMailByIdMarkUnread({
          client,
          path: { cityName, id },
          headers: GC_MUTATION_HEADERS,
          ...(query === undefined ? {} : { query }),
        }) as Promise<SupervisorResult<OkResponseBody>>,
        'gc supervisor mail mark-unread response was empty',
      );
    },
    archiveMail(cityName, id, query) {
      return unwrapSupervisorResult<OkResponseBody>(
        postV0CityByCityNameMailByIdArchive({
          client,
          path: { cityName, id },
          headers: GC_MUTATION_HEADERS,
          ...(query === undefined ? {} : { query }),
        }) as Promise<SupervisorResult<OkResponseBody>>,
        'gc supervisor mail archive response was empty',
      );
    },
    replyMail(cityName, id, body, query) {
      return unwrapSupervisorResult<Message>(
        postSupervisorMailReply({
          client,
          path: { cityName, id },
          headers: GC_MUTATION_HEADERS,
          body,
          ...(query === undefined ? {} : { query }),
        }) as Promise<SupervisorResult<Message>>,
        'gc supervisor mail reply response was empty',
      );
    },
    cityEventStreamUrl(cityName, afterSeq) {
      return supervisorUrl(
        baseUrl,
        `/v0/city/${encodeURIComponent(cityName)}/events/stream`,
        afterSeq === undefined ? undefined : { after_seq: afterSeq },
      );
    },
    sessionStreamUrl(cityName, sessionId, after) {
      return supervisorUrl(
        baseUrl,
        `/v0/city/${encodeURIComponent(cityName)}/session/${encodeURIComponent(sessionId)}/stream`,
        after === undefined ? undefined : { after },
      );
    },
    listSessions(cityName) {
      return unwrapSupervisorResult<ListBodySessionResponse>(
        getV0CityByCityNameSessions({
          client,
          path: { cityName },
        }) as Promise<SupervisorResult<ListBodySessionResponse>>,
        'gc supervisor sessions response was empty',
      );
    },
    sessionPending(cityName, sessionId) {
      return unwrapSupervisorResult<SessionPendingResponse>(
        getV0CityByCityNameSessionByIdPending({
          client,
          path: { cityName, id: sessionId },
        }) as Promise<SupervisorResult<SessionPendingResponse>>,
        'gc supervisor session pending response was empty',
      );
    },
    respondSession(cityName, sessionId, body) {
      return unwrapSupervisorResult<RespondSessionResponse>(
        postSupervisorSessionRespond({
          client,
          path: { cityName, id: sessionId },
          headers: GC_MUTATION_HEADERS,
          body,
        }) as Promise<SupervisorResult<RespondSessionResponse>>,
        'gc supervisor session respond response was empty',
      );
    },
    sessionTranscript(cityName, sessionId) {
      return unwrapSupervisorResult<SessionTranscriptGetResponse>(
        getV0CityByCityNameSessionByIdTranscript({
          client,
          path: { cityName, id: sessionId },
          query: { format: 'conversation' },
        }) as Promise<SupervisorResult<SessionTranscriptGetResponse>>,
        'gc supervisor transcript response was empty',
      );
    },
    workflowRun(cityName, workflowId, query) {
      return unwrapSupervisorResult<WorkflowSnapshotResponse>(
        getV0CityByCityNameWorkflowByWorkflowId({
          client,
          path: { cityName, workflow_id: workflowId },
          ...(query === undefined ? {} : { query }),
        }) as Promise<SupervisorResult<WorkflowSnapshotResponse>>,
        'gc supervisor workflow response was empty',
      );
    },
    formulaDetail(cityName, name, query) {
      return unwrapSupervisorResult<FormulaDetailResponse>(
        getV0CityByCityNameFormulasByName({
          client,
          path: { cityName, name },
          query,
        }) as Promise<SupervisorResult<FormulaDetailResponse>>,
        'gc supervisor formula detail response was empty',
      );
    },
    mutationHeaders() {
      return { ...GC_MUTATION_HEADERS };
    },
  };
}

export function supervisorApi(): SupervisorApi {
  if (testSupervisorApi !== null) return testSupervisorApi;
  defaultSupervisorApi ??= createSupervisorApi();
  return defaultSupervisorApi;
}

export function supervisorApiForRequestBudget(timeoutMs: number): SupervisorApi {
  if (testSupervisorApi !== null) return testSupervisorApi;
  const normalizedTimeoutMs = supervisorTimeoutMs(timeoutMs);
  const cached = requestBudgetSupervisorApis.get(normalizedTimeoutMs);
  if (cached !== undefined) return cached;
  const api = createSupervisorApi({ timeoutMs: normalizedTimeoutMs });
  requestBudgetSupervisorApis.set(normalizedTimeoutMs, api);
  return api;
}

export function setSupervisorApiForTests(api: SupervisorApi): void {
  testSupervisorApi = api;
}

export function resetSupervisorApiForTests(): void {
  testSupervisorApi = null;
  defaultSupervisorApi = null;
  requestBudgetSupervisorApis.clear();
}

function supervisorTimeoutMs(value: number | undefined): number {
  return typeof value === 'number' && Number.isFinite(value) && value > 0
    ? value
    : SUPERVISOR_REQUEST_TIMEOUT_MS;
}

function withSupervisorTimeout(fetchImpl: typeof fetch, timeoutMs: number): typeof fetch {
  return async (input, init) => {
    const controller = new AbortController();
    const timeoutError = new SupervisorApiError(
      undefined,
      `gc supervisor request timed out after ${timeoutMs}ms`,
      undefined,
    );
    const parentSignal = requestSignal(input, init);
    if (parentSignal?.aborted) {
      controller.abort(parentSignal.reason);
    }
    const abortFromParent = (): void => controller.abort(parentSignal?.reason);
    parentSignal?.addEventListener('abort', abortFromParent, { once: true });

    let timeout: ReturnType<typeof setTimeout> | undefined;
    const timeoutPromise = new Promise<Response>((_resolve, reject) => {
      timeout = setTimeout(() => {
        controller.abort(timeoutError);
        reject(timeoutError);
      }, timeoutMs);
    });

    const request = new Request(input, { ...init, signal: controller.signal });
    const fetchPromise = fetchImpl(request);
    try {
      return await Promise.race([fetchPromise, timeoutPromise]);
    } finally {
      if (timeout !== undefined) clearTimeout(timeout);
      parentSignal?.removeEventListener('abort', abortFromParent);
    }
  };
}

function requestSignal(
  input: RequestInfo | URL,
  init: RequestInit | undefined,
): AbortSignal | null {
  if (init?.signal !== undefined) return init.signal;
  return input instanceof Request ? input.signal : null;
}
