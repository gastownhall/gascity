package specgen

import (
	"fmt"
	"sort"
	"strings"
)

// GenerateAsyncAPI produces the AsyncAPI 2.6.0 YAML spec from the registry.
func GenerateAsyncAPI(r *Registry) string {
	var b strings.Builder

	b.WriteString(`asyncapi: 2.6.0
id: urn:gascity:supervisor-websocket:v1alpha1
defaultContentType: application/json
info:
  title: Gas City Supervisor WebSocket Protocol
  version: 1.0.0
  description: >
    Shared websocket protocol for the city API server and supervisor mux at
    GET /v0/ws. The server sends hello/response/error/event envelopes. Clients
    send request envelopes.

    This spec is auto-generated from the Go type registry. Do not edit manually.
    Regenerate with: go generate ./internal/api/specgen/...
servers:
  city:
    url: localhost
    protocol: ws
    description: Per-city websocket endpoint.
  supervisor:
    url: localhost
    protocol: ws
    description: Supervisor websocket endpoint.
channels:
  /v0/ws:
    description: Bidirectional websocket channel for requests, responses, and subscriptions.
    publish:
      operationId: sendRequest
      message:
        $ref: '#/components/messages/RequestEnvelopeMessage'
    subscribe:
      operationId: receiveServerEnvelope
      message:
        oneOf:
          - $ref: '#/components/messages/HelloEnvelopeMessage'
          - $ref: '#/components/messages/ResponseEnvelopeMessage'
          - $ref: '#/components/messages/ErrorEnvelopeMessage'
          - $ref: '#/components/messages/EventEnvelopeMessage'
`)

	// Components section.
	b.WriteString("components:\n")
	b.WriteString("  messages:\n")
	b.WriteString(messageSchemas())
	b.WriteString("  schemas:\n")
	b.WriteString(envelopeSchemas(r))
	b.WriteString(actionPayloadSchemas(r))

	return b.String()
}

func messageSchemas() string {
	return `    HelloEnvelopeMessage:
      name: HelloEnvelope
      payload:
        $ref: '#/components/schemas/HelloEnvelope'
    RequestEnvelopeMessage:
      name: RequestEnvelope
      payload:
        $ref: '#/components/schemas/RequestEnvelope'
    ResponseEnvelopeMessage:
      name: ResponseEnvelope
      payload:
        $ref: '#/components/schemas/ResponseEnvelope'
    ErrorEnvelopeMessage:
      name: ErrorEnvelope
      payload:
        $ref: '#/components/schemas/ErrorEnvelope'
    EventEnvelopeMessage:
      name: EventEnvelope
      payload:
        $ref: '#/components/schemas/EventEnvelope'
`
}

func envelopeSchemas(r *Registry) string {
	actions := r.ActionNames()
	var b strings.Builder

	// Hello envelope.
	b.WriteString(`    HelloEnvelope:
      type: object
      required: [type]
      properties:
        type:
          type: string
          enum: [hello]
        protocol:
          type: string
        server_role:
          type: string
          enum: [city, supervisor]
        read_only:
          type: boolean
        capabilities:
          type: array
          items:
            type: string
`)

	// Request envelope.
	b.WriteString("    RequestEnvelope:\n")
	b.WriteString("      type: object\n")
	b.WriteString("      required: [type, id, action]\n")
	b.WriteString("      properties:\n")
	b.WriteString("        type:\n")
	b.WriteString("          type: string\n")
	b.WriteString("          enum: [request]\n")
	b.WriteString("        id:\n")
	b.WriteString("          type: string\n")
	b.WriteString("        action:\n")
	b.WriteString("          type: string\n")
	b.WriteString("          enum:\n")
	for _, name := range actions {
		fmt.Fprintf(&b, "            - %s\n", name)
	}
	b.WriteString("        payload:\n")
	b.WriteString("          type: object\n")
	b.WriteString("          description: Action-specific request payload. Schema varies by action.\n")
	b.WriteString("        scope:\n")
	b.WriteString("          type: object\n")
	b.WriteString("          properties:\n")
	b.WriteString("            city:\n")
	b.WriteString("              type: string\n")
	b.WriteString("        idempotency_key:\n")
	b.WriteString("          type: string\n")
	b.WriteString("          description: Deduplication key for mutation replay.\n")
	b.WriteString("        watch:\n")
	b.WriteString("          type: object\n")
	b.WriteString("          description: Blocking query parameters.\n")
	b.WriteString("          properties:\n")
	b.WriteString("            index:\n")
	b.WriteString("              type: integer\n")
	b.WriteString("              description: Block until server index exceeds this value.\n")
	b.WriteString("            wait:\n")
	b.WriteString("              type: string\n")
	b.WriteString("              description: Maximum wait duration (e.g. '30s').\n")

	// Response envelope.
	b.WriteString(`    ResponseEnvelope:
      type: object
      required: [type, id]
      properties:
        type:
          type: string
          enum: [response]
        id:
          type: string
        index:
          type: integer
          description: Server event index for watch semantics.
        result:
          type: object
          description: Action-specific response payload.
`)

	// Error envelope.
	b.WriteString(`    ErrorEnvelope:
      type: object
      required: [type, code, message]
      properties:
        type:
          type: string
          enum: [error]
        id:
          type: string
        code:
          type: string
        message:
          type: string
        details:
          type: array
          items:
            type: object
            properties:
              field:
                type: string
              message:
                type: string
`)

	// Event envelope.
	b.WriteString(`    EventEnvelope:
      type: object
      required: [type, subscription_id]
      properties:
        type:
          type: string
          enum: [event]
        subscription_id:
          type: string
        event_type:
          type: string
        index:
          type: integer
        cursor:
          type: string
        payload:
          type: object
`)

	return b.String()
}

func actionPayloadSchemas(r *Registry) string {
	var b strings.Builder
	actions := r.Actions()

	// Group actions by domain for readability.
	domains := map[string][]ActionDef{}
	for _, a := range actions {
		parts := strings.SplitN(a.Action, ".", 2)
		domain := parts[0]
		domains[domain] = append(domains[domain], a)
	}

	domainNames := make([]string, 0, len(domains))
	for d := range domains {
		domainNames = append(domainNames, d)
	}
	sort.Strings(domainNames)

	// Emit action descriptions as x-actions extension.
	b.WriteString("    # ── Action catalog (x-gc-actions) ──\n")
	b.WriteString("    # Auto-generated from Go type registry.\n")
	b.WriteString("    x-gc-actions:\n")
	b.WriteString("      type: object\n")
	b.WriteString("      description: Catalog of all WebSocket actions.\n")
	b.WriteString("      properties:\n")
	for _, domain := range domainNames {
		for _, a := range domains[domain] {
			fmt.Fprintf(&b, "        %s:\n", a.Action)
			fmt.Fprintf(&b, "          description: %s\n", a.Description)
			if a.IsMutation {
				b.WriteString("          x-mutation: true\n")
			}
			if a.RequestType != nil {
				schema := JSONSchema(a.RequestType)
				writeInlineSchema(&b, "          x-request-schema:\n", schema, 12)
			}
			if a.ResponseType != nil {
				schema := JSONSchema(a.ResponseType)
				writeInlineSchema(&b, "          x-response-schema:\n", schema, 12)
			}
		}
	}

	return b.String()
}

func writeInlineSchema(b *strings.Builder, header string, schema map[string]any, indent int) {
	if schema == nil || len(schema) == 0 {
		return
	}
	b.WriteString(header)
	writeYAMLMap(b, schema, indent)
}

func writeYAMLMap(b *strings.Builder, m map[string]any, indent int) {
	prefix := strings.Repeat(" ", indent)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		v := m[k]
		switch val := v.(type) {
		case string:
			fmt.Fprintf(b, "%s%s: %s\n", prefix, k, val)
		case bool:
			fmt.Fprintf(b, "%s%s: %v\n", prefix, k, val)
		case int, int64, uint64, float64:
			fmt.Fprintf(b, "%s%s: %v\n", prefix, k, val)
		case []string:
			fmt.Fprintf(b, "%s%s:\n", prefix, k)
			for _, s := range val {
				fmt.Fprintf(b, "%s  - %s\n", prefix, s)
			}
		case map[string]any:
			fmt.Fprintf(b, "%s%s:\n", prefix, k)
			writeYAMLMap(b, val, indent+2)
		default:
			fmt.Fprintf(b, "%s%s: %v\n", prefix, k, val)
		}
	}
}

// GenerateOpenAPI produces the OpenAPI 3.1.0 YAML spec for HTTP-only endpoints.
func GenerateOpenAPI() string {
	return `openapi: 3.1.0
info:
  title: Gas City HTTP API
  version: 1.0.0
  description: >
    HTTP-only operational endpoints for the Gas City API server and supervisor.
    All domain operations use WebSocket at GET /v0/ws — see the AsyncAPI spec
    at GET /v0/asyncapi.yaml for the full WS protocol.

    This spec is auto-generated. Do not edit manually.
    Regenerate with: go generate ./internal/api/specgen/...

    These HTTP endpoints exist because their consumers (load balancers, health
    probes, process managers) cannot use WebSocket upgrade.
servers:
  - url: http://localhost:8080
    description: Default standalone API server
paths:
  /health:
    get:
      operationId: healthCheck
      summary: Health probe
      description: >
        Returns 200 OK when the API server is running. Used by Kubernetes
        liveness probes and load balancers.
      responses:
        '200':
          description: Server is healthy
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/HealthResponse'
  /v0/readiness:
    get:
      operationId: readinessCheck
      summary: Readiness probe
      description: Returns 200 when the server is ready to accept requests.
      responses:
        '200':
          description: Server is ready
          content:
            application/json:
              schema:
                type: object
                properties:
                  status:
                    type: string
                    enum: [ok]
  /v0/provider-readiness:
    get:
      operationId: providerReadinessCheck
      summary: Provider readiness probe
      description: Returns 200 when at least one AI provider is configured and reachable.
      responses:
        '200':
          description: At least one provider is ready
          content:
            application/json:
              schema:
                type: object
                properties:
                  status:
                    type: string
                    enum: [ok]
        '503':
          description: No providers are ready
  /v0/city:
    post:
      operationId: createCity
      summary: Register a city with the supervisor
      description: >
        Process manager endpoint for registering a new city directory.
        Not part of the client API.
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [path]
              properties:
                path:
                  type: string
                  description: Absolute filesystem path to the city directory
                name:
                  type: string
                  description: Optional display name override
      responses:
        '201':
          description: City registered
        '409':
          description: City already registered
  /v0/ws:
    get:
      operationId: websocketUpgrade
      summary: WebSocket upgrade endpoint
      description: >
        Upgrades to WebSocket for the full domain API. See the AsyncAPI spec
        at GET /v0/asyncapi.yaml for the complete protocol documentation.
      responses:
        '101':
          description: Switching Protocols (WebSocket upgrade)
  /v0/asyncapi.yaml:
    get:
      operationId: getAsyncAPISpec
      summary: AsyncAPI specification
      description: Returns the AsyncAPI YAML spec for the WebSocket protocol.
      responses:
        '200':
          description: AsyncAPI spec
          content:
            text/yaml:
              schema:
                type: string
  /v0/openapi.yaml:
    get:
      operationId: getOpenAPISpec
      summary: OpenAPI specification
      description: Returns this OpenAPI YAML spec for the HTTP endpoints.
      responses:
        '200':
          description: OpenAPI spec
          content:
            text/yaml:
              schema:
                type: string
components:
  schemas:
    HealthResponse:
      type: object
      properties:
        status:
          type: string
          enum: [ok]
        version:
          type: string
        uptime_sec:
          type: integer
`
}
