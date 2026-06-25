declare module '@hey-api/client-fetch' {
  export type Auth = {
    scheme?: string;
    type: string;
  };

  export type QuerySerializerOptions = {
    allowReserved?: boolean;
    array?: {
      explode?: boolean;
      style?: 'form' | 'spaceDelimited' | 'pipeDelimited';
    };
    object?: {
      explode?: boolean;
      style?: 'deepObject' | 'form';
    };
  };

  export type ResponseStyle = 'data' | 'fields';

  export interface Config<T extends ClientOptions = ClientOptions> extends Omit<
    RequestInit,
    'body' | 'headers' | 'method'
  > {
    auth?: unknown;
    baseUrl?: T['baseUrl'];
    bodySerializer?: ((body: unknown) => unknown) | null;
    fetch?: typeof fetch;
    headers?:
      | RequestInit['headers']
      | Record<
          string,
          string | number | boolean | (string | number | boolean)[] | null | undefined | unknown
        >;
    parseAs?: 'arrayBuffer' | 'auto' | 'blob' | 'formData' | 'json' | 'stream' | 'text';
    querySerializer?: ((query: Record<string, unknown>) => string) | QuerySerializerOptions;
    requestValidator?: (data: unknown) => Promise<unknown>;
    responseStyle?: ResponseStyle;
    responseTransformer?: (data: unknown) => Promise<unknown>;
    responseValidator?: (data: unknown) => Promise<unknown>;
    throwOnError?: T['throwOnError'];
  }

  export interface RequestOptions<
    TData = unknown,
    TResponseStyle extends ResponseStyle = 'fields',
    ThrowOnError extends boolean = boolean,
    Url extends string = string,
  > extends Config<{
    responseStyle: TResponseStyle;
    throwOnError: ThrowOnError;
  }> {
    body?: unknown;
    onRequest?: (url: string, init: RequestInit) => Request | Promise<Request>;
    onSseError?: (error: unknown) => void;
    onSseEvent?: (event: StreamEvent<TData>) => void;
    path?: Record<string, unknown>;
    query?: Record<string, unknown>;
    security?: ReadonlyArray<Auth>;
    sseDefaultRetryDelay?: number;
    sseMaxRetryAttempts?: number;
    sseMaxRetryDelay?: number;
    url: Url;
  }

  export interface StreamEvent<TData = unknown> {
    data: TData;
    event?: string;
    id?: string;
    retry?: number;
  }

  export type ServerSentEventsResult<TData = unknown> = {
    close: () => void;
    events: AsyncIterable<StreamEvent<TData>>;
  };

  export type RequestResult<
    TData = unknown,
    TError = unknown,
    ThrowOnError extends boolean = boolean,
    TResponseStyle extends ResponseStyle = 'fields',
  > = ThrowOnError extends true
    ? Promise<
        TResponseStyle extends 'data'
          ? TData extends Record<string, unknown>
            ? TData[keyof TData]
            : TData
          : {
              data: TData extends Record<string, unknown> ? TData[keyof TData] : TData;
              request: Request;
              response: Response;
            }
      >
    : Promise<
        TResponseStyle extends 'data'
          ? (TData extends Record<string, unknown> ? TData[keyof TData] : TData) | undefined
          : (
              | {
                  data: TData extends Record<string, unknown> ? TData[keyof TData] : TData;
                  error: undefined;
                }
              | {
                  data: undefined;
                  error: TError extends Record<string, unknown> ? TError[keyof TError] : TError;
                }
            ) & {
              request?: Request;
              response?: Response;
            }
      >;

  export interface ClientOptions {
    baseUrl?: string;
    responseStyle?: ResponseStyle;
    throwOnError?: boolean;
  }

  export interface TDataShape {
    body?: unknown;
    headers?: unknown;
    path?: unknown;
    query?: unknown;
    url: string;
  }

  export type Options<
    TData extends TDataShape = TDataShape,
    ThrowOnError extends boolean = boolean,
    TResponse = unknown,
    TResponseStyle extends ResponseStyle = 'fields',
  > = Omit<
    RequestOptions<TResponse, TResponseStyle, ThrowOnError>,
    'body' | 'path' | 'query' | 'url'
  > &
    ([TData] extends [never] ? unknown : Omit<TData, 'url'>);

  export type OptionsLegacyParser<
    TData = unknown,
    ThrowOnError extends boolean = boolean,
    TResponseStyle extends ResponseStyle = 'fields',
  > = TData extends { body?: unknown }
    ? TData extends { headers?: unknown }
      ? Omit<RequestOptions<unknown, TResponseStyle, ThrowOnError>, 'body' | 'headers' | 'url'> &
          TData
      : Omit<RequestOptions<unknown, TResponseStyle, ThrowOnError>, 'body' | 'url'> &
          TData &
          Pick<RequestOptions<unknown, TResponseStyle, ThrowOnError>, 'headers'>
    : TData extends { headers?: unknown }
      ? Omit<RequestOptions<unknown, TResponseStyle, ThrowOnError>, 'headers' | 'url'> &
          TData &
          Pick<RequestOptions<unknown, TResponseStyle, ThrowOnError>, 'body'>
      : Omit<RequestOptions<unknown, TResponseStyle, ThrowOnError>, 'url'> & TData;

  type MethodFn = <
    TData = unknown,
    TError = unknown,
    ThrowOnError extends boolean = false,
    TResponseStyle extends ResponseStyle = 'fields',
  >(
    options: Omit<RequestOptions<TData, TResponseStyle, ThrowOnError>, 'method'>,
  ) => RequestResult<TData, TError, ThrowOnError, TResponseStyle>;

  type SseFn = <
    TData = unknown,
    _TError = unknown,
    ThrowOnError extends boolean = false,
    TResponseStyle extends ResponseStyle = 'fields',
  >(
    options: Omit<RequestOptions<never, TResponseStyle, ThrowOnError>, 'method'>,
  ) => Promise<ServerSentEventsResult<TData>>;

  export type Client = {
    buildUrl: <
      TData extends {
        body?: unknown;
        path?: Record<string, unknown>;
        query?: Record<string, unknown>;
        url: string;
      },
    >(
      options: TData & Options<TData>,
    ) => string;
    connect: MethodFn;
    delete: MethodFn;
    get: MethodFn;
    getConfig: () => Config;
    head: MethodFn;
    interceptors: unknown;
    options: MethodFn;
    patch: MethodFn;
    post: MethodFn;
    put: MethodFn;
    request: <
      TData = unknown,
      TError = unknown,
      ThrowOnError extends boolean = false,
      TResponseStyle extends ResponseStyle = 'fields',
    >(
      options: Omit<RequestOptions<TData, TResponseStyle, ThrowOnError>, 'method'> &
        Pick<Required<RequestOptions<TData, TResponseStyle, ThrowOnError>>, 'method'>,
    ) => RequestResult<TData, TError, ThrowOnError, TResponseStyle>;
    setConfig: (config: Config) => Config;
    sse: {
      connect: SseFn;
      delete: SseFn;
      get: SseFn;
      head: SseFn;
      options: SseFn;
      patch: SseFn;
      post: SseFn;
      put: SseFn;
      trace: SseFn;
    };
    trace: MethodFn;
  };

  export function createClient(config?: Config): Client;
  export function createConfig<T extends ClientOptions = ClientOptions>(
    override?: Config<ClientOptions & T>,
  ): Config<Required<ClientOptions> & T>;
  export function buildClientParams(args: ReadonlyArray<unknown>, fields: unknown): unknown;
  export function formDataBodySerializer(body: unknown): FormData;
  export function jsonBodySerializer(body: unknown): string;
  export function urlSearchParamsBodySerializer(body: unknown): URLSearchParams;
}
