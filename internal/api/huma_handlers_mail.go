package api

import (
	"context"
	"errors"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
)

// humaHandleMailList is the Huma-typed handler for GET /v0/mail.
func (s *Server) humaHandleMailList(ctx context.Context, input *MailListInput) (*ListOutput[mail.Message], error) {
	bp := input.toBlockingParams()
	if bp.isBlocking() {
		waitForChange(ctx, s.state.EventProvider(), bp)
	}

	pp := pageParams{Limit: 50}
	if input.Limit > 0 {
		pp.Limit = input.Limit
		if pp.Limit > maxPaginationLimit {
			pp.Limit = maxPaginationLimit
		}
	}
	if input.Cursor != "" {
		pp.Offset = decodeCursor(input.Cursor)
		pp.IsPaging = true
	}

	agent := input.Agent
	status := input.Status
	rig := input.Rig
	index := s.latestIndex()

	switch status {
	case "", "unread":
		if rig != "" {
			mp := s.state.MailProvider(rig)
			if mp == nil {
				return &ListOutput[mail.Message]{
					Index: index,
					Body:  ListBody[mail.Message]{Items: []mail.Message{}, Total: 0},
				}, nil
			}
			msgs, err := mp.Inbox(agent)
			if err != nil {
				return nil, huma.Error500InternalServerError(err.Error())
			}
			if msgs == nil {
				msgs = []mail.Message{}
			}
			msgs = tagRig(msgs, rig)
			if !pp.IsPaging {
				total := len(msgs)
				if pp.Limit < len(msgs) {
					msgs = msgs[:pp.Limit]
				}
				return &ListOutput[mail.Message]{
					Index: index,
					Body:  ListBody[mail.Message]{Items: msgs, Total: total},
				}, nil
			}
			page, total, nextCursor := paginate(msgs, pp)
			if page == nil {
				page = []mail.Message{}
			}
			return &ListOutput[mail.Message]{
				Index: index,
				Body:  ListBody[mail.Message]{Items: page, Total: total, NextCursor: nextCursor},
			}, nil
		}

		providers := s.state.MailProviders()
		var allMsgs []mail.Message
		for _, name := range sortedProviderNames(providers) {
			msgs, err := providers[name].Inbox(agent)
			if err != nil {
				return nil, huma.Error500InternalServerError("mail provider " + name + ": " + err.Error())
			}
			allMsgs = append(allMsgs, tagRig(msgs, name)...)
		}
		if allMsgs == nil {
			allMsgs = []mail.Message{}
		}
		if !pp.IsPaging {
			total := len(allMsgs)
			if pp.Limit < len(allMsgs) {
				allMsgs = allMsgs[:pp.Limit]
			}
			return &ListOutput[mail.Message]{
				Index: index,
				Body:  ListBody[mail.Message]{Items: allMsgs, Total: total},
			}, nil
		}
		page, total, nextCursor := paginate(allMsgs, pp)
		if page == nil {
			page = []mail.Message{}
		}
		return &ListOutput[mail.Message]{
			Index: index,
			Body:  ListBody[mail.Message]{Items: page, Total: total, NextCursor: nextCursor},
		}, nil

	case "all":
		if rig != "" {
			mp := s.state.MailProvider(rig)
			if mp == nil {
				return &ListOutput[mail.Message]{
					Index: index,
					Body:  ListBody[mail.Message]{Items: []mail.Message{}, Total: 0},
				}, nil
			}
			msgs, err := mp.All(agent)
			if err != nil {
				return nil, huma.Error500InternalServerError(err.Error())
			}
			if msgs == nil {
				msgs = []mail.Message{}
			}
			msgs = tagRig(msgs, rig)
			if !pp.IsPaging {
				total := len(msgs)
				if pp.Limit < len(msgs) {
					msgs = msgs[:pp.Limit]
				}
				return &ListOutput[mail.Message]{
					Index: index,
					Body:  ListBody[mail.Message]{Items: msgs, Total: total},
				}, nil
			}
			page, total, nextCursor := paginate(msgs, pp)
			if page == nil {
				page = []mail.Message{}
			}
			return &ListOutput[mail.Message]{
				Index: index,
				Body:  ListBody[mail.Message]{Items: page, Total: total, NextCursor: nextCursor},
			}, nil
		}

		providers := s.state.MailProviders()
		var allMsgs []mail.Message
		for _, name := range sortedProviderNames(providers) {
			msgs, err := providers[name].All(agent)
			if err != nil {
				return nil, huma.Error500InternalServerError("mail provider " + name + ": " + err.Error())
			}
			allMsgs = append(allMsgs, tagRig(msgs, name)...)
		}
		if allMsgs == nil {
			allMsgs = []mail.Message{}
		}
		if !pp.IsPaging {
			total := len(allMsgs)
			if pp.Limit < len(allMsgs) {
				allMsgs = allMsgs[:pp.Limit]
			}
			return &ListOutput[mail.Message]{
				Index: index,
				Body:  ListBody[mail.Message]{Items: allMsgs, Total: total},
			}, nil
		}
		page, total, nextCursor := paginate(allMsgs, pp)
		if page == nil {
			page = []mail.Message{}
		}
		return &ListOutput[mail.Message]{
			Index: index,
			Body:  ListBody[mail.Message]{Items: page, Total: total, NextCursor: nextCursor},
		}, nil

	default:
		return nil, huma.Error400BadRequest("unsupported status filter: " + status + "; supported: unread, all")
	}
}

// humaHandleMailGet is the Huma-typed handler for GET /v0/mail/{id}.
func (s *Server) humaHandleMailGet(_ context.Context, input *MailGetInput) (*IndexOutput[mail.Message], error) {
	id := input.ID
	rig := input.Rig
	mp, resolvedRig, err := s.findMailProviderForMessage(id, rig)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	if mp == nil {
		return nil, huma.Error404NotFound("message " + id + " not found")
	}

	msg, err := mp.Get(id)
	if err != nil {
		if errors.Is(err, mail.ErrNotFound) {
			return nil, huma.Error404NotFound(err.Error())
		}
		return nil, huma.Error500InternalServerError(err.Error())
	}
	msg.Rig = resolvedRig
	return &IndexOutput[mail.Message]{
		Index: s.latestIndex(),
		Body:  msg,
	}, nil
}

// humaHandleMailSend is the Huma-typed handler for POST /v0/mail.
// Body validation (To and Subject required, minLength:"1") is enforced by
// the framework from MailSendInput's struct tags.
func (s *Server) humaHandleMailSend(_ context.Context, input *MailSendInput) (*IndexOutput[mail.Message], error) {
	// Resolve recipient against configured agents.
	resolved, resolveErr := mail.ResolveRecipient(input.Body.To, agentEntries(s.state.Config()))
	if resolveErr != nil {
		return nil, huma.Error400BadRequest(resolveErr.Error())
	}

	mp := s.findMailProvider(input.Body.Rig)
	if mp == nil {
		return nil, huma.Error400BadRequest("no mail provider available")
	}

	// Idempotency check — scope by method+path to prevent cross-endpoint collisions.
	idemKey := ""
	var bodyHash string
	if input.IdempotencyKey != "" {
		idemKey = "POST:/v0/mail:" + input.IdempotencyKey
		bodyHash = hashBody(input.Body)
		existing, found := s.idem.reserve(idemKey, bodyHash)
		if found {
			if existing.bodyHash != bodyHash {
				return nil, huma.Error422UnprocessableEntity("idempotency_mismatch: Idempotency-Key reused with different request body")
			}
			if existing.pending {
				return nil, huma.Error409Conflict("in_flight: request with this Idempotency-Key is already in progress")
			}
			// Replay cached typed response (Fix 3l).
			if msg, ok := replayAs[mail.Message](existing); ok {
				return &IndexOutput[mail.Message]{
					Index: s.latestIndex(),
					Body:  msg,
				}, nil
			}
		}
	}

	msg, err := mp.Send(input.Body.From, resolved, input.Body.Subject, input.Body.Body)
	if err != nil {
		s.idem.unreserve(idemKey)
		return nil, huma.Error500InternalServerError(err.Error())
	}
	msg.Rig = input.Body.Rig
	s.idem.storeResponse(idemKey, bodyHash, msg)
	s.recordMailEvent(events.MailSent, input.Body.From, msg.ID, input.Body.Rig, &msg)

	return &IndexOutput[mail.Message]{
		Index: s.latestIndex(),
		Body:  msg,
	}, nil
}

// humaHandleMailCount is the Huma-typed handler for GET /v0/mail/count.
func (s *Server) humaHandleMailCount(_ context.Context, input *MailCountInput) (*MailCountOutput, error) {
	agentName := input.Agent
	rig := input.Rig

	if rig != "" {
		mp := s.state.MailProvider(rig)
		if mp == nil {
			resp := &MailCountOutput{}
			resp.Body.Total = 0
			resp.Body.Unread = 0
			return resp, nil
		}
		total, unread, err := mp.Count(agentName)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		resp := &MailCountOutput{}
		resp.Body.Total = total
		resp.Body.Unread = unread
		return resp, nil
	}

	// Aggregate across all rigs (deduplicated by provider identity).
	providers := s.state.MailProviders()
	var totalAll, unreadAll int
	for _, name := range sortedProviderNames(providers) {
		total, unread, err := providers[name].Count(agentName)
		if err != nil {
			return nil, huma.Error500InternalServerError("mail provider " + name + ": " + err.Error())
		}
		totalAll += total
		unreadAll += unread
	}
	resp := &MailCountOutput{}
	resp.Body.Total = totalAll
	resp.Body.Unread = unreadAll
	return resp, nil
}

// humaHandleMailThread is the Huma-typed handler for GET /v0/mail/thread/{id}.
func (s *Server) humaHandleMailThread(_ context.Context, input *MailThreadInput) (*ListOutput[mail.Message], error) {
	threadID := input.ID
	rig := input.Rig
	index := s.latestIndex()

	if rig != "" {
		mp := s.state.MailProvider(rig)
		if mp == nil {
			return nil, huma.Error404NotFound("rig " + rig + " not found")
		}
		msgs, err := mp.Thread(threadID)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if msgs == nil {
			msgs = []mail.Message{}
		}
		msgs = tagRig(msgs, rig)
		return &ListOutput[mail.Message]{
			Index: index,
			Body:  ListBody[mail.Message]{Items: msgs, Total: len(msgs)},
		}, nil
	}

	// Aggregate thread messages across all providers.
	providers := s.state.MailProviders()
	var allMsgs []mail.Message
	for _, name := range sortedProviderNames(providers) {
		msgs, err := providers[name].Thread(threadID)
		if err != nil {
			return nil, huma.Error500InternalServerError("mail provider " + name + ": " + err.Error())
		}
		allMsgs = append(allMsgs, tagRig(msgs, name)...)
	}
	if allMsgs == nil {
		allMsgs = []mail.Message{}
	}
	return &ListOutput[mail.Message]{
		Index: index,
		Body:  ListBody[mail.Message]{Items: allMsgs, Total: len(allMsgs)},
	}, nil
}

// humaHandleMailRead is the Huma-typed handler for POST /v0/mail/{id}/read.
func (s *Server) humaHandleMailRead(_ context.Context, input *MailReadInput) (*OKResponse, error) {
	id := input.ID
	rig := input.Rig
	mp, resolvedRig, err := s.findMailProviderForMessage(id, rig)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	if mp == nil {
		return nil, huma.Error404NotFound("message " + id + " not found")
	}
	if err := mp.MarkRead(id); err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	s.recordMailEvent(events.MailMarkedRead, "api", id, resolvedRig, nil)
	resp := &OKResponse{}
	resp.Body.Status = "read"
	return resp, nil
}

// humaHandleMailMarkUnread is the Huma-typed handler for POST /v0/mail/{id}/mark-unread.
func (s *Server) humaHandleMailMarkUnread(_ context.Context, input *MailMarkUnreadInput) (*OKResponse, error) {
	id := input.ID
	rig := input.Rig
	mp, resolvedRig, err := s.findMailProviderForMessage(id, rig)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	if mp == nil {
		return nil, huma.Error404NotFound("message " + id + " not found")
	}
	if err := mp.MarkUnread(id); err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	s.recordMailEvent(events.MailMarkedUnread, "api", id, resolvedRig, nil)
	resp := &OKResponse{}
	resp.Body.Status = "unread"
	return resp, nil
}

// humaHandleMailArchive is the Huma-typed handler for POST /v0/mail/{id}/archive.
func (s *Server) humaHandleMailArchive(_ context.Context, input *MailArchiveInput) (*OKResponse, error) {
	id := input.ID
	rig := input.Rig
	mp, resolvedRig, err := s.findMailProviderForMessage(id, rig)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	if mp == nil {
		return nil, huma.Error404NotFound("message " + id + " not found")
	}
	if err := mp.Archive(id); err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	s.recordMailEvent(events.MailArchived, "api", id, resolvedRig, nil)
	resp := &OKResponse{}
	resp.Body.Status = "archived"
	return resp, nil
}

// humaHandleMailReply is the Huma-typed handler for POST /v0/mail/{id}/reply.
func (s *Server) humaHandleMailReply(_ context.Context, input *MailReplyInput) (*IndexOutput[mail.Message], error) {
	id := input.ID
	rig := input.Rig

	mp, resolvedRig, mpErr := s.findMailProviderForMessage(id, rig)
	if mpErr != nil {
		return nil, huma.Error500InternalServerError(mpErr.Error())
	}
	if mp == nil {
		return nil, huma.Error404NotFound("message " + id + " not found")
	}

	msg, err := mp.Reply(id, input.Body.From, input.Body.Subject, input.Body.Body)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	msg.Rig = resolvedRig
	s.recordMailEvent(events.MailReplied, input.Body.From, msg.ID, resolvedRig, &msg)

	return &IndexOutput[mail.Message]{
		Index: s.latestIndex(),
		Body:  msg,
	}, nil
}

// humaHandleMailDelete is the Huma-typed handler for DELETE /v0/mail/{id}.
func (s *Server) humaHandleMailDelete(_ context.Context, input *MailDeleteInput) (*OKResponse, error) {
	id := input.ID
	rig := input.Rig
	mp, resolvedRig, err := s.findMailProviderForMessage(id, rig)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	if mp == nil {
		return nil, huma.Error404NotFound("message " + id + " not found")
	}
	if err := mp.Delete(id); err != nil {
		if errors.Is(err, mail.ErrNotFound) || errors.Is(err, beads.ErrNotFound) {
			return nil, huma.Error404NotFound("message " + id + " not found")
		}
		return nil, huma.Error500InternalServerError(err.Error())
	}
	s.recordMailEvent(events.MailDeleted, "api", id, resolvedRig, nil)
	resp := &OKResponse{}
	resp.Body.Status = "deleted"
	return resp, nil
}
