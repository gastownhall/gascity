import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { api } from "../api";
import { syncCityScopeFromLocation } from "../state";
import { getOptions, invalidateOptions } from "./options";

describe("options cache", () => {
  beforeEach(() => {
    window.history.pushState({}, "", "/dashboard?city=mc-city");
    syncCityScopeFromLocation();
    invalidateOptions();
  });

  afterEach(() => {
    vi.restoreAllMocks();
    invalidateOptions();
    window.history.pushState({}, "", "/dashboard");
    syncCityScopeFromLocation();
  });

  it("uses configured agents as compose recipients", async () => {
    const getSpy = vi.spyOn(api, "GET").mockImplementation(async (path: string) => {
      if (path === "/v0/city/{cityName}/config") {
        return {
          data: {
            agents: [
              { name: "mayor", display_name: "The Mayor" },
            ],
          },
          error: undefined,
          request: undefined,
          response: undefined,
        } as never;
      }
      if (path === "/v0/city/{cityName}/rigs") {
        return { data: { items: [] }, error: undefined, request: undefined, response: undefined } as never;
      }
      if (path === "/v0/city/{cityName}/beads") {
        return { data: { items: [] }, error: undefined, request: undefined, response: undefined } as never;
      }
      if (path === "/v0/city/{cityName}/mail") {
        return { data: { items: [] }, error: undefined, request: undefined, response: undefined } as never;
      }
      throw new Error(`unexpected GET ${path}`);
    });

    const options = await getOptions(true);

    expect(options.agents).toEqual(["mayor"]);
    expect(options.sessions).toEqual([
      { id: "mayor", label: "mayor", recipient: "mayor" },
    ]);
    expect(getSpy).not.toHaveBeenCalledWith(
      "/v0/city/{cityName}/sessions",
      expect.anything(),
    );
  });
});
