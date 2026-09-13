package api

// OptionalParam wraps a query-parameter value with a presence flag so
// handlers can distinguish "parameter absent" from "parameter present
// with zero value" without reading raw URL values.
//
// This is the Huma-documented idiom for presence detection on query
// parameters. See https://huma.rocks/features/request-inputs/ and the
// sample in Huma's own huma_test.go. Huma v2 does not support pointer
// query parameters (see github.com/danielgtaylor/huma issue #288),
// which is why this wrapper exists instead of *T.
//
// Usage:
//
//	type Input struct {
//	    Cursor OptionalParam[string] `query:"cursor"`
//	}
//
//	func h(ctx context.Context, in *Input) (*Out, error) {
//	    if in.Cursor.IsSet { ... }
//	}
//
// The spec emits the underlying T's schema (not the wrapper's), so the
// wire contract is identical to a plain query:"..." field: the only
// difference is server-side presence detection. This stays within
// engdocs/architecture/api-control-plane.md §3.5.1: the handler does
// not read undeclared URL keys, only its own declared field.

import (
	"reflect"

	"github.com/danielgtaylor/huma/v2"
)

// OptionalParam holds a query-parameter value and a flag indicating
// whether the client explicitly supplied the parameter.
type OptionalParam[T any] struct {
	Value T
	IsSet bool
}

// Schema returns the schema for the wrapped type so the OpenAPI spec
// emits a plain T rather than a struct shape.
func (o OptionalParam[T]) Schema(r huma.Registry) *huma.Schema {
	return huma.SchemaFromType(r, reflect.TypeOf(o.Value))
}

// Receiver exposes the Value field so Huma's parameter binder writes
// directly into it during request parsing.
func (o *OptionalParam[T]) Receiver() reflect.Value {
	return reflect.ValueOf(o).Elem().Field(0)
}

// OnParamSet is called by Huma after parsing; the isSet flag reflects
// whether the raw parameter was present in the request.
func (o *OptionalParam[T]) OnParamSet(isSet bool, _ any) {
	o.IsSet = isSet
}
