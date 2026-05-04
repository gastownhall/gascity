package api

// Framework-level OpenAPI decoration.
//
// Some wire contract spans every operation rather than any one input
// or output struct: the X-GC-Request-Id response header, written by
// withRequestID middleware on every response, is one such case. OpenAPI
// 3.1 has no mechanism to declare a header "globally"; the canonical
// pattern is to define the header once in components.headers and $ref
// it from each operation's responses (see
// speakeasy.com/openapi/responses/headers).
//
// registerFrameworkHeaders walks the registered OpenAPI document once
// after all routes are registered and adds the $ref entries. Handlers
// don't need to know or declare anything; the middleware remains the
// single source of enforcement, and the spec describes it accurately.

import (
	"github.com/danielgtaylor/huma/v2"
)

const (
	requestIDHeaderName  = "X-GC-Request-Id"
	requestIDHeaderRef   = "#/components/headers/" + requestIDHeaderName
	requestIDDescription = "Opaque per-response identifier assigned by the server for log correlation. Every response carries this header."
)

// registerFrameworkHeaders registers reusable response headers in
// components.headers and adds $ref pointers to every registered
// operation's responses. Call once after all routes are registered.
func registerFrameworkHeaders(api huma.API) {
	spec := api.OpenAPI()
	if spec == nil {
		return
	}
	if spec.Components == nil {
		spec.Components = &huma.Components{}
	}
	if spec.Components.Headers == nil {
		spec.Components.Headers = map[string]*huma.Header{}
	}
	if _, ok := spec.Components.Headers[requestIDHeaderName]; !ok {
		spec.Components.Headers[requestIDHeaderName] = &huma.Header{
			Description: requestIDDescription,
			Schema: &huma.Schema{
				Type:        "string",
				Description: requestIDDescription,
			},
		}
	}
	if spec.Paths == nil {
		return
	}
	for _, item := range spec.Paths {
		if item == nil {
			continue
		}
		ops := []*huma.Operation{
			item.Get, item.Put, item.Post, item.Patch, item.Delete,
			item.Head, item.Options, item.Trace,
		}
		for _, op := range ops {
			if op == nil || op.Responses == nil {
				continue
			}
			for _, resp := range op.Responses {
				if resp == nil {
					continue
				}
				if resp.Headers == nil {
					resp.Headers = map[string]*huma.Param{}
				}
				if _, ok := resp.Headers[requestIDHeaderName]; ok {
					continue
				}
				resp.Headers[requestIDHeaderName] = &huma.Param{
					Ref: requestIDHeaderRef,
				}
			}
		}
	}
}
