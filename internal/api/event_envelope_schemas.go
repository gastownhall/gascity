package api

import (
	"reflect"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/events"
)

type typedEventStreamEnvelopeSchema struct{}

func (typedEventStreamEnvelopeSchema) Schema(r huma.Registry) *huma.Schema {
	return registerTypedEventEnvelopeSchema(r, typedEventEnvelopeSchemaConfig{
		name:        "TypedEventStreamEnvelope",
		title:       "Typed city event stream envelope",
		description: "Discriminated union of city event stream envelopes. Each variant constrains the envelope type and payload schema together.",
	})
}

type typedTaggedEventStreamEnvelopeSchema struct{}

func (typedTaggedEventStreamEnvelopeSchema) Schema(r huma.Registry) *huma.Schema {
	return registerTypedEventEnvelopeSchema(r, typedEventEnvelopeSchemaConfig{
		name:        "TypedTaggedEventStreamEnvelope",
		title:       "Typed supervisor event stream envelope",
		description: "Discriminated union of supervisor event stream envelopes. Each variant constrains the envelope type and payload schema together and includes the source city.",
		includeCity: true,
	})
}

type typedEventEnvelopeSchemaConfig struct {
	name        string
	title       string
	description string
	includeCity bool
}

type typedEventEnvelopeVariant struct {
	eventType   string
	payloadType reflect.Type
}

func registerEventEnvelopeCompatibilitySchemas(r huma.Registry) {
	r.Schema(reflect.TypeOf(eventStreamEnvelope{}), true, "EventStreamEnvelope")
	r.Schema(reflect.TypeOf(taggedEventStreamEnvelope{}), true, "TaggedEventStreamEnvelope")
	_ = typedEventStreamEnvelopeSchema{}.Schema(r)
	_ = typedTaggedEventStreamEnvelopeSchema{}.Schema(r)
}

func registerTypedEventEnvelopeSchema(r huma.Registry, cfg typedEventEnvelopeSchemaConfig) *huma.Schema {
	if _, ok := r.Map()[cfg.name]; !ok {
		variants := typedEventEnvelopeVariants()
		oneOf := make([]*huma.Schema, 0, len(variants))
		mapping := make(map[string]string, len(variants))
		for _, variant := range variants {
			variantName := cfg.name + eventTypeSchemaSuffix(variant.eventType)
			ref := schemaRefPrefix + variantName
			if _, ok := r.Map()[variantName]; !ok {
				r.Map()[variantName] = typedEventEnvelopeVariantSchema(r, variant, cfg)
			}
			oneOf = append(oneOf, &huma.Schema{Ref: ref})
			mapping[variant.eventType] = ref
		}
		customName := cfg.name + "Custom"
		customRef := schemaRefPrefix + customName
		if _, ok := r.Map()[customName]; !ok {
			r.Map()[customName] = customEventEnvelopeVariantSchema(r, cfg)
		}
		oneOf = append(oneOf, &huma.Schema{Ref: customRef})
		r.Map()[cfg.name] = &huma.Schema{
			Title:       cfg.title,
			Description: cfg.description,
			OneOf:       oneOf,
			Discriminator: &huma.Discriminator{
				PropertyName: "type",
				Mapping:      mapping,
			},
		}
	}
	return &huma.Schema{Ref: schemaRefPrefix + cfg.name}
}

func typedEventEnvelopeVariants() []typedEventEnvelopeVariant {
	registered := events.RegisteredPayloadTypes()
	variants := make([]typedEventEnvelopeVariant, 0, len(events.KnownEventTypes))
	for _, eventType := range events.KnownEventTypes {
		payload, ok := registered[eventType]
		if !ok {
			panic("api: known event type has no registered payload: " + eventType)
		}
		payloadType := reflect.TypeOf(payload)
		if payloadType == nil {
			panic("api: known event type has nil registered payload: " + eventType)
		}
		variants = append(variants, typedEventEnvelopeVariant{
			eventType:   eventType,
			payloadType: payloadType,
		})
	}
	sort.Slice(variants, func(i, j int) bool {
		return variants[i].eventType < variants[j].eventType
	})
	return variants
}

func typedEventEnvelopeVariantSchema(r huma.Registry, variant typedEventEnvelopeVariant, cfg typedEventEnvelopeSchemaConfig) *huma.Schema {
	properties := map[string]*huma.Schema{
		"seq": {
			Type:    huma.TypeInteger,
			Format:  "int64",
			Minimum: float64Ptr(0),
		},
		"type": {
			Type: huma.TypeString,
			Extensions: map[string]any{
				"const": variant.eventType,
			},
		},
		"ts": {
			Type:   huma.TypeString,
			Format: "date-time",
		},
		"actor": {
			Type: huma.TypeString,
		},
		"subject": {
			Type: huma.TypeString,
		},
		"message": {
			Type: huma.TypeString,
		},
		"workflow": r.Schema(reflect.TypeOf(workflowEventProjection{}), true, "WorkflowEventProjection"),
		"payload":  r.Schema(variant.payloadType, true, variant.payloadType.Name()),
	}
	required := []string{"seq", "type", "ts", "actor", "payload"}
	if cfg.includeCity {
		properties["city"] = &huma.Schema{Type: huma.TypeString}
		required = append(required, "city")
	}
	return &huma.Schema{
		Title:                cfg.name + " " + variant.eventType,
		Type:                 huma.TypeObject,
		AdditionalProperties: false,
		Properties:           properties,
		Required:             required,
	}
}

func customEventEnvelopeVariantSchema(r huma.Registry, cfg typedEventEnvelopeSchemaConfig) *huma.Schema {
	knownTypes := make([]any, 0, len(events.KnownEventTypes))
	for _, eventType := range events.KnownEventTypes {
		knownTypes = append(knownTypes, eventType)
	}
	properties := map[string]*huma.Schema{
		"seq": {
			Type:    huma.TypeInteger,
			Format:  "int64",
			Minimum: float64Ptr(0),
		},
		"type": {
			Type: huma.TypeString,
			Not:  &huma.Schema{Enum: knownTypes},
		},
		"ts": {
			Type:   huma.TypeString,
			Format: "date-time",
		},
		"actor": {
			Type: huma.TypeString,
		},
		"subject": {
			Type: huma.TypeString,
		},
		"message": {
			Type: huma.TypeString,
		},
		"workflow": r.Schema(reflect.TypeOf(workflowEventProjection{}), true, "WorkflowEventProjection"),
		"payload":  {},
	}
	required := []string{"seq", "type", "ts", "actor", "payload"}
	if cfg.includeCity {
		properties["city"] = &huma.Schema{Type: huma.TypeString}
		required = append(required, "city")
	}
	return &huma.Schema{
		Title:                cfg.name + " custom",
		Type:                 huma.TypeObject,
		AdditionalProperties: false,
		Properties:           properties,
		Required:             required,
	}
}

func eventTypeSchemaSuffix(eventType string) string {
	parts := strings.FieldsFunc(eventType, func(r rune) bool {
		return r == '.' || r == '_' || r == '-'
	})
	var out strings.Builder
	for _, part := range parts {
		if part == "" {
			continue
		}
		out.WriteString(strings.ToUpper(part[:1]))
		out.WriteString(part[1:])
	}
	return out.String()
}

func float64Ptr(v float64) *float64 {
	return &v
}
