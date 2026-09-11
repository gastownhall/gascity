import { activeCityOrThrow } from '../api/cityBase';
import type {
  GetV0CityByCityNameEventsData,
  ListBodyWireEvent,
  TypedEventStreamEnvelope,
} from 'gas-city-dashboard-shared/gc-supervisor';
import { supervisorApi } from './client';

const EVENT_FETCH_LIMIT = 100;
export const DEFAULT_EVENT_WINDOW = '24h';

export type SupervisorEventItem = TypedEventStreamEnvelope;
export type SupervisorEventQuery = NonNullable<GetV0CityByCityNameEventsData['query']>;

export type SupervisorEventList = Omit<ListBodyWireEvent, 'items' | 'total'> & {
  items: SupervisorEventItem[];
  total: number;
};

export async function listSupervisorEvents(
  query: SupervisorEventQuery = {},
): Promise<SupervisorEventList> {
  const cityName = activeCityOrThrow('list supervisor events');
  const list = await supervisorApi().listEvents(cityName, {
    limit: EVENT_FETCH_LIMIT,
    since: DEFAULT_EVENT_WINDOW,
    ...query,
  });
  const items = list.items ?? [];
  items.sort((a, b) => b.seq - a.seq);
  return {
    ...list,
    items,
    total: Number(list.total),
  };
}
