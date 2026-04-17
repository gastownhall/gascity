// Entry point — real implementation follows in later phases.
// This scaffold only proves the build pipeline + typed client work
// end-to-end against the live supervisor.

import { api, cityScope } from "./api";

async function boot(): Promise<void> {
  const city = cityScope();
  const { data, error } = await api.GET("/v0/cities");
  if (error) {
    // eslint-disable-next-line no-console
    console.error("GET /v0/cities failed", error);
    return;
  }
  // eslint-disable-next-line no-console
  console.info("dashboard scaffold online", { city, cities: data });
}

void boot();
