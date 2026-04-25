import { defineConfig } from "@hey-api/openapi-ts";

// @hey-api/openapi-ts generates both the typed REST SDK and the typed
// SSE handlers from the supervisor's committed OpenAPI 3.1 spec. The
// output drives every API call and SSE stream the dashboard makes —
// path construction, request/response typing, event discrimination,
// retry, and auth headers all flow through generated code.
//
// See engdocs/architecture/api-control-plane.md §6 "Tooling landscape" for the rationale.
export default defineConfig({
  input: "../../../../internal/api/openapi.json",
  output: {
    path: "./src/generated",
    postProcess: ["prettier"],
  },
  plugins: [
    "@hey-api/client-fetch",
    "@hey-api/typescript",
    "@hey-api/sdk",
  ],
});
