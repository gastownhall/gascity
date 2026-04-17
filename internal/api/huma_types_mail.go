package api

// Per-domain Huma input/output types for the mail handler
// group. Split out of the original huma_types.go; mirrors the layout
// of huma_handlers_mail.go.

// --- Mail types ---

// MailListInput is the Huma input for GET /v0/city/{cityName}/mail.
type MailListInput struct {
	CityScope
	BlockingParam
	PaginationParam
	Agent  string `query:"agent" required:"false" doc:"Filter by agent name."`
	Status string `query:"status" required:"false" doc:"Filter by status (unread, all)."`
	Rig    string `query:"rig" required:"false" doc:"Filter by rig name."`
}

// MailGetInput is the Huma input for GET /v0/city/{cityName}/mail/{id}.
type MailGetInput struct {
	CityScope
	ID  string `path:"id" doc:"Message ID."`
	Rig string `query:"rig" required:"false" doc:"Rig hint for O(1) lookup."`
}

// MailSendInput is the Huma input for POST /v0/city/{cityName}/mail.
type MailSendInput struct {
	CityScope
	IdempotencyKey string `header:"Idempotency-Key" required:"false" doc:"Idempotency key for safe retries."`
	Body           struct {
		Rig     string `json:"rig,omitempty" doc:"Rig name."`
		From    string `json:"from,omitempty" doc:"Sender name."`
		To      string `json:"to" doc:"Recipient name." minLength:"1"`
		Subject string `json:"subject" doc:"Message subject." minLength:"1"`
		Body    string `json:"body,omitempty" doc:"Message body."`
	}
}

// MailReadInput is the Huma input for POST /v0/city/{cityName}/mail/{id}/read.
type MailReadInput struct {
	CityScope
	ID  string `path:"id" doc:"Message ID."`
	Rig string `query:"rig" required:"false" doc:"Rig hint."`
}

// MailMarkUnreadInput is the Huma input for POST /v0/city/{cityName}/mail/{id}/mark-unread.
type MailMarkUnreadInput struct {
	CityScope
	ID  string `path:"id" doc:"Message ID."`
	Rig string `query:"rig" required:"false" doc:"Rig hint."`
}

// MailArchiveInput is the Huma input for POST /v0/city/{cityName}/mail/{id}/archive.
type MailArchiveInput struct {
	CityScope
	ID  string `path:"id" doc:"Message ID."`
	Rig string `query:"rig" required:"false" doc:"Rig hint."`
}

// MailReplyInput is the Huma input for POST /v0/city/{cityName}/mail/{id}/reply.
type MailReplyInput struct {
	CityScope
	ID   string `path:"id" doc:"Message ID."`
	Rig  string `query:"rig" required:"false" doc:"Rig hint."`
	Body struct {
		From    string `json:"from,omitempty" doc:"Sender name."`
		Subject string `json:"subject,omitempty" doc:"Reply subject."`
		Body    string `json:"body,omitempty" doc:"Reply body."`
	}
}

// MailDeleteInput is the Huma input for DELETE /v0/city/{cityName}/mail/{id}.
type MailDeleteInput struct {
	CityScope
	ID  string `path:"id" doc:"Message ID."`
	Rig string `query:"rig" required:"false" doc:"Rig hint."`
}

// MailThreadInput is the Huma input for GET /v0/city/{cityName}/mail/thread/{id}.
type MailThreadInput struct {
	CityScope
	ID  string `path:"id" doc:"Thread ID."`
	Rig string `query:"rig" required:"false" doc:"Filter by rig."`
}

// MailCountInput is the Huma input for GET /v0/city/{cityName}/mail/count.
type MailCountInput struct {
	CityScope
	Agent string `query:"agent" required:"false" doc:"Filter by agent name."`
	Rig   string `query:"rig" required:"false" doc:"Filter by rig name."`
}

// MailCountOutput is the response body for GET /v0/mail/count.
type MailCountOutput struct {
	Body struct {
		Total  int `json:"total" doc:"Total message count."`
		Unread int `json:"unread" doc:"Unread message count."`
	}
}

