// Public surface of the static "test-city" supervisor fixture. Consumed by the
// Playwright route installer (scripts/fixtures/install-supervisor-fixtures.mjs)
// and by any test that needs a deterministic, seeded supervisor snapshot for
// the dashboard's `/gc-supervisor/*`-backed tabs.

export {
  buildTestCitySupervisorData,
  TEST_CITY_NAME,
  TEST_CITY_RIGS,
  type TestCitySupervisorData,
} from './data.js';
export {
  matchTestCitySupervisorRequest,
  renderTestCityEventStream,
  type FixtureResponse,
} from './match.js';
