package api

import (
	"reflect"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/session"
)

// This file names the string-enum schemas used by the supervisor API so
// that they appear as first-class entries in components.schemas instead
// of being inlined as bare `type: string` fields. The pattern is:
//
//  1. Declare an API-package wrapper struct per enum (submitIntentSchema,
//     etc.) that implements huma.SchemaProvider. The wrapper registers
//     its named schema once in the registry's components map and returns
//     a $ref to it.
//  2. Call RegisterTypeAlias at API creation time so huma uses the
//     wrapper's schema whenever it encounters the domain enum type
//     (session.SubmitIntent, extmsg.ConversationKind, ...).
//
// This keeps the huma dependency inside the api package — domain
// packages do not have to know about huma.

const schemaRefPrefix = "#/components/schemas/"

type submitIntentSchema struct{}

func (submitIntentSchema) Schema(r huma.Registry) *huma.Schema {
	return registerNamedEnum(r, "SubmitIntent",
		"Semantic delivery choice for a user message on a session submit request.",
		string(session.SubmitIntentDefault),
		string(session.SubmitIntentFollowUp),
		string(session.SubmitIntentInterruptNow),
	)
}

type conversationKindSchema struct{}

func (conversationKindSchema) Schema(r huma.Registry) *huma.Schema {
	return registerNamedEnum(r, "ConversationKind",
		"Shape of a conversation.",
		string(extmsg.ConversationDM),
		string(extmsg.ConversationRoom),
		string(extmsg.ConversationThread),
	)
}

type transcriptMessageKindSchema struct{}

func (transcriptMessageKindSchema) Schema(r huma.Registry) *huma.Schema {
	return registerNamedEnum(r, "TranscriptMessageKind",
		"Direction of a transcript entry.",
		string(extmsg.TranscriptMessageInbound),
		string(extmsg.TranscriptMessageOutbound),
	)
}

type transcriptProvenanceSchema struct{}

func (transcriptProvenanceSchema) Schema(r huma.Registry) *huma.Schema {
	return registerNamedEnum(r, "TranscriptProvenance",
		"Provenance of a transcript entry (freshly observed vs. replayed from persisted history).",
		string(extmsg.TranscriptProvenanceLive),
		string(extmsg.TranscriptProvenanceHydrated),
	)
}

type bindingStatusSchema struct{}

func (bindingStatusSchema) Schema(r huma.Registry) *huma.Schema {
	return registerNamedEnum(r, "BindingStatus",
		"Lifecycle state of a session binding.",
		string(extmsg.BindingActive),
		string(extmsg.BindingEnded),
	)
}

func registerNamedEnum(r huma.Registry, name, description string, values ...string) *huma.Schema {
	if _, ok := r.Map()[name]; !ok {
		enum := make([]any, len(values))
		for i, v := range values {
			enum[i] = v
		}
		r.Map()[name] = &huma.Schema{
			Type:        huma.TypeString,
			Description: description,
			Enum:        enum,
		}
	}
	return &huma.Schema{Ref: schemaRefPrefix + name}
}

// registerEnumAliases redirects the schema generator to use the wrapper
// schema types above whenever it encounters one of the domain enum
// types. Called from the supervisor API setup.
func registerEnumAliases(r huma.Registry) {
	r.RegisterTypeAlias(reflect.TypeOf(session.SubmitIntent("")), reflect.TypeOf(submitIntentSchema{}))
	r.RegisterTypeAlias(reflect.TypeOf(extmsg.ConversationKind("")), reflect.TypeOf(conversationKindSchema{}))
	r.RegisterTypeAlias(reflect.TypeOf(extmsg.TranscriptMessageKind("")), reflect.TypeOf(transcriptMessageKindSchema{}))
	r.RegisterTypeAlias(reflect.TypeOf(extmsg.TranscriptProvenance("")), reflect.TypeOf(transcriptProvenanceSchema{}))
	r.RegisterTypeAlias(reflect.TypeOf(extmsg.BindingStatus("")), reflect.TypeOf(bindingStatusSchema{}))
}
