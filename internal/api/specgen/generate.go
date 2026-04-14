package specgen

import (
	"net/http"

	asyncapi "github.com/swaggest/go-asyncapi/reflector/asyncapi-2.4.0"
	asyncapispec "github.com/swaggest/go-asyncapi/spec-2.4.0"
	"github.com/swaggest/openapi-go/openapi3"
)

// GenerateAsyncAPI produces a valid AsyncAPI 2.4.0 spec from the registry
// using the swaggest/go-asyncapi reflector for proper JSON Schema generation.
// The EnvelopeTypes must be populated with the actual protocol types from the
// api package — these are the REAL types that go over the wire, not mirrors.
func GenerateAsyncAPI(r *Registry, envelopes EnvelopeTypes) ([]byte, error) {
	api := asyncapispec.AsyncAPI{}
	api.ID = "urn:gascity:supervisor-websocket:v1alpha1"
	api.DefaultContentType = "application/json"
	api.Info.Title = "Gas City Supervisor WebSocket Protocol"
	api.Info.Version = "1.0.0"
	api.Info.Description =
		"WebSocket protocol for the city API server and supervisor mux at " +
			"GET /v0/ws. Auto-generated from the Go type registry via " +
			"go generate ./internal/api/specgen/..."

	api.AddServer("city", asyncapispec.Server{
		URL:         "localhost",
		Protocol:    "ws",
		Description: "Per-city websocket endpoint.",
	})
	api.AddServer("supervisor", asyncapispec.Server{
		URL:         "localhost",
		Protocol:    "ws",
		Description: "Supervisor websocket endpoint.",
	})

	ref := asyncapi.Reflector{}
	ref.Schema = &api

	// Register the main /v0/ws channel with wire protocol envelope types.
	// These are the actual message shapes that go over the wire — the
	// complete contract for any client implementing the protocol.
	ref.AddChannel(asyncapi.ChannelInfo{
		Name: "/v0/ws",
		Publish: &asyncapi.MessageSample{
			MessageEntity: asyncapispec.MessageEntity{
				Description: "Client-to-server request envelope",
				Summary:     "Request Envelope",
			},
			MessageSample: envelopes.Request,
		},
		Subscribe: &asyncapi.MessageSample{
			MessageEntity: asyncapispec.MessageEntity{
				Description: "Server-to-client message (response, error, event, or hello)",
				Summary:     "Server Envelope",
			},
			MessageSample: envelopes.Response,
		},
	})

	// Register each envelope type as a separate channel so the reflector
	// generates full JSON Schema components for all of them.
	ref.AddChannel(asyncapi.ChannelInfo{
		Name: "protocol/hello",
		Subscribe: &asyncapi.MessageSample{
			MessageEntity: asyncapispec.MessageEntity{
				Description: "Sent by the server immediately after WebSocket upgrade",
				Summary:     "Hello Envelope",
			},
			MessageSample: envelopes.Hello,
		},
	})
	ref.AddChannel(asyncapi.ChannelInfo{
		Name: "protocol/error",
		Subscribe: &asyncapi.MessageSample{
			MessageEntity: asyncapispec.MessageEntity{
				Description: "Sent by the server when a request fails",
				Summary:     "Error Envelope",
			},
			MessageSample: envelopes.Error,
		},
	})
	ref.AddChannel(asyncapi.ChannelInfo{
		Name: "protocol/event",
		Subscribe: &asyncapi.MessageSample{
			MessageEntity: asyncapispec.MessageEntity{
				Description: "Sent by the server for subscription events",
				Summary:     "Event Envelope",
			},
			MessageSample: envelopes.Event,
		},
	})
	ref.AddChannel(asyncapi.ChannelInfo{
		Name: "protocol/subscription-start",
		Publish: &asyncapi.MessageSample{
			MessageEntity: asyncapispec.MessageEntity{
				Description: "Start an event or session stream subscription",
				Summary:     "Subscription Start Request",
			},
			MessageSample: envelopes.SubscriptionStart,
		},
	})
	ref.AddChannel(asyncapi.ChannelInfo{
		Name: "protocol/subscription-stop",
		Publish: &asyncapi.MessageSample{
			MessageEntity: asyncapispec.MessageEntity{
				Description: "Stop a subscription by ID",
				Summary:     "Subscription Stop Request",
			},
			MessageSample: envelopes.SubscriptionStop,
		},
	})

	// Register per-action channels. Each action gets at least a request
	// channel (even with no typed payload) so it appears in the spec.
	// Actions with typed payloads get full JSON Schema via the reflector.
	for _, a := range r.Actions() {
		if a.RequestSample != nil {
			ref.AddChannel(asyncapi.ChannelInfo{
				Name: "actions/" + a.Action + "/request",
				Publish: &asyncapi.MessageSample{
					MessageEntity: asyncapispec.MessageEntity{
						Description: a.Action + " request payload",
						Summary:     a.Description,
					},
					MessageSample: a.RequestSample,
				},
			})
		} else {
			// No typed payload — add a minimal channel so the action
			// appears in the spec with its description.
			if api.Channels == nil {
				api.Channels = map[string]asyncapispec.ChannelItem{}
			}
			api.Channels["actions/"+a.Action+"/request"] = asyncapispec.ChannelItem{
				Description: a.Description,
			}
		}
		if a.ResponseSample != nil {
			ref.AddChannel(asyncapi.ChannelInfo{
				Name: "actions/" + a.Action + "/response",
				Subscribe: &asyncapi.MessageSample{
					MessageEntity: asyncapispec.MessageEntity{
						Description: a.Action + " response payload",
						Summary:     a.Description,
					},
					MessageSample: a.ResponseSample,
				},
			})
		}
	}

	return api.MarshalYAML()
}

// GenerateOpenAPI produces a valid OpenAPI 3.1.0 spec for the HTTP-only
// endpoints using the swaggest/openapi-go reflector.
func GenerateOpenAPI() ([]byte, error) {
	ref := openapi3.NewReflector()
	ref.Spec.Info.Title = "Gas City HTTP API"
	ref.Spec.Info.Version = "1.0.0"
	ref.Spec.Info.WithDescription(
		"HTTP-only operational endpoints for the Gas City API server. " +
			"All domain operations use WebSocket at GET /v0/ws. " +
			"Auto-generated from Go types via go generate ./internal/api/specgen/...")

	ref.Spec.WithServers(openapi3.Server{URL: "http://localhost:8080", Description: strPtr("Default standalone API server")})

	// GET /health
	addSimpleOp(ref, http.MethodGet, "/health", "Health probe",
		"Returns 200 OK when the API server is running.",
		struct {
			Status string `json:"status"`
		}{}, http.StatusOK)

	// GET /v0/readiness
	addSimpleOp(ref, http.MethodGet, "/v0/readiness", "Readiness probe",
		"Returns 200 when the server is ready to accept requests.",
		struct {
			Status string `json:"status"`
		}{}, http.StatusOK)

	// GET /v0/provider-readiness
	addSimpleOp(ref, http.MethodGet, "/v0/provider-readiness", "Provider readiness probe",
		"Returns 200 when at least one AI provider is configured and reachable.",
		struct {
			Status string `json:"status"`
		}{}, http.StatusOK)

	// POST /v0/city
	{
		op := openapi3.Operation{}
		op.WithSummary("Register a city with the supervisor")
		op.WithDescription("Process manager endpoint for registering a new city directory.")
		oc := openapi3.OperationContext{
			Operation:  &op,
			HTTPMethod: http.MethodPost,
			Input: struct {
				Path string `json:"path" description:"Absolute filesystem path to the city directory"`
				Name string `json:"name,omitempty" description:"Optional display name override"`
			}{},
			Output:     nil,
			HTTPStatus: http.StatusCreated,
		}
		_ = ref.SetupRequest(oc)
		ref.Spec.AddOperation(http.MethodPost, "/v0/city", op)
	}

	// GET /v0/ws
	{
		op := openapi3.Operation{}
		op.WithSummary("WebSocket upgrade endpoint")
		op.WithDescription("Upgrades to WebSocket. See GET /v0/asyncapi.yaml for the protocol documentation.")
		ref.Spec.AddOperation(http.MethodGet, "/v0/ws", op)
	}

	// GET /v0/asyncapi.yaml
	{
		op := openapi3.Operation{}
		op.WithSummary("AsyncAPI specification")
		op.WithDescription("Returns the AsyncAPI YAML spec for the WebSocket protocol.")
		_ = ref.SetStringResponse(&op, http.StatusOK, "text/yaml")
		ref.Spec.AddOperation(http.MethodGet, "/v0/asyncapi.yaml", op)
	}

	// GET /v0/openapi.yaml
	{
		op := openapi3.Operation{}
		op.WithSummary("OpenAPI specification")
		op.WithDescription("Returns this OpenAPI YAML spec for the HTTP endpoints.")
		_ = ref.SetStringResponse(&op, http.StatusOK, "text/yaml")
		ref.Spec.AddOperation(http.MethodGet, "/v0/openapi.yaml", op)
	}

	return ref.Spec.MarshalYAML()
}

func addSimpleOp(ref *openapi3.Reflector, method, path, summary, desc string, resp interface{}, status int) {
	op := openapi3.Operation{}
	op.WithSummary(summary)
	op.WithDescription(desc)
	_ = ref.SetJSONResponse(&op, resp, status)
	ref.Spec.AddOperation(method, path, op)
}

func strPtr(s string) *string { return &s }
