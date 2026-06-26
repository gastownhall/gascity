package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// mailReadDeadline is shorter than the API client's 10s timeout so typed
// store_slow problem details can reach the CLI before transport timeout.
var mailReadDeadline = 8 * time.Second

type mailReadTimeoutError struct {
	d time.Duration
}

func (e *mailReadTimeoutError) Error() string {
	return fmt.Sprintf("%s: mail read timed out after %s", StoreSlowErrorCode, e.d)
}

type mailReadResult[T any] struct {
	value T
	err   error
}

type mailReadCounts struct {
	Total  int
	Unread int
}

type mailGetResult struct {
	Message mail.Message
	Rig     string
	Found   bool
}

type mailProviderReadResult[T any] struct {
	name  string
	value T
	err   error
}

// withMailReadDeadline bounds mail store reads whose provider interface has
// no context parameter. If the deadline fires, the provider goroutine may keep
// running until the store call returns; its result is discarded through the
// buffered channel.
func withMailReadDeadline[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var zero T
	deadline := mailReadDeadline
	if deadline <= 0 {
		return fn()
	}
	ch := make(chan mailReadResult[T], 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				ch <- mailReadResult[T]{err: fmt.Errorf("mail provider read panicked: %v", recovered)}
			}
		}()
		value, err := fn()
		ch <- mailReadResult[T]{value: value, err: err}
	}()
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	select {
	case res := <-ch:
		return res.value, res.err
	case <-ctx.Done():
		return zero, ctx.Err()
	case <-timer.C:
		return zero, &mailReadTimeoutError{d: deadline}
	}
}

// withMailProviderReadDeadline runs aggregate provider reads under one shared
// deadline, keeping all-rig API responses inside the API client's timeout.
func withMailProviderReadDeadline[T any](ctx context.Context, providers map[string]mail.Provider, fn func(mail.Provider) (T, error)) []mailProviderReadResult[T] {
	names := sortedProviderNames(providers)
	if len(names) == 0 {
		return nil
	}

	ch := make(chan mailProviderReadResult[T], len(names))
	for _, name := range names {
		name := name
		provider := providers[name]
		go func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					ch <- mailProviderReadResult[T]{name: name, err: fmt.Errorf("mail provider read panicked: %v", recovered)}
				}
			}()
			value, err := fn(provider)
			ch <- mailProviderReadResult[T]{name: name, value: value, err: err}
		}()
	}

	results := make(map[string]mailReadResult[T], len(names))
	pending := make(map[string]struct{}, len(names))
	for _, name := range names {
		pending[name] = struct{}{}
	}

	deadline := mailReadDeadline
	var timer *time.Timer
	var timeout <-chan time.Time
	if deadline > 0 {
		timer = time.NewTimer(deadline)
		defer timer.Stop()
		timeout = timer.C
	}

	for len(pending) > 0 {
		select {
		case res := <-ch:
			if _, ok := pending[res.name]; !ok {
				continue
			}
			delete(pending, res.name)
			results[res.name] = mailReadResult[T]{value: res.value, err: res.err}
		case <-ctx.Done():
			var zero T
			for _, name := range names {
				if _, ok := pending[name]; ok {
					results[name] = mailReadResult[T]{value: zero, err: ctx.Err()}
				}
			}
			return orderedMailProviderReadResults(names, results)
		case <-timeout:
			var zero T
			for _, name := range names {
				if _, ok := pending[name]; ok {
					results[name] = mailReadResult[T]{value: zero, err: &mailReadTimeoutError{d: deadline}}
				}
			}
			return orderedMailProviderReadResults(names, results)
		}
	}

	return orderedMailProviderReadResults(names, results)
}

func orderedMailProviderReadResults[T any](names []string, results map[string]mailReadResult[T]) []mailProviderReadResult[T] {
	ordered := make([]mailProviderReadResult[T], 0, len(names))
	for _, name := range names {
		res := results[name]
		ordered = append(ordered, mailProviderReadResult[T]{name: name, value: res.value, err: res.err})
	}
	return ordered
}

func mailReadAPIError(err error) error {
	var timeoutErr *mailReadTimeoutError
	if errors.As(err, &timeoutErr) {
		return huma.Error503ServiceUnavailable(timeoutErr.Error())
	}
	return huma.Error500InternalServerError(err.Error())
}

func allMailProvidersFailedError(partialErrs []string, storeSlow bool) error {
	detail := "all mail providers failed: " + strings.Join(partialErrs, "; ")
	if storeSlow {
		detail = "store_slow: " + detail
	}
	return huma.Error503ServiceUnavailable(detail)
}

// humaHandleMailList is the Huma-typed handler for GET /v0/mail.
func (s *Server) humaHandleMailList(ctx context.Context, input *MailListInput) (*MailListOutput, error) {
	bp := input.toBlockingParams()
	if bp.isBlocking() {
		waitForChange(ctx, s.state.EventProvider(), bp)
	}

	cityStore := s.state.CityBeadStore()
	if err := cacheLiveOr503(cityStore); err != nil {
		return nil, err
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

	agents := s.resolveMailQueryRecipientsWithContext(ctx, input.Agent)
	status := input.Status
	rig := input.Rig
	index := s.latestIndex()
	cacheAge := cacheAgeSeconds(cityStore)

	switch status {
	case "", "unread":
		if rig != "" {
			mp := s.state.MailProvider(rig)
			if mp == nil {
				return &MailListOutput{
					Index:     index,
					CacheAgeS: cacheAge,
					Body:      MailListBody{Items: []mail.Message{}, Total: 0},
				}, nil
			}
			msgs, err := withMailReadDeadline(ctx, func() ([]mail.Message, error) {
				return mailInboxForRecipients(mp, agents)
			})
			if err != nil {
				return nil, mailReadAPIError(err)
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
				return &MailListOutput{
					Index:     index,
					CacheAgeS: cacheAge,
					Body:      MailListBody{Items: msgs, Total: total},
				}, nil
			}
			page, total, nextCursor := paginate(msgs, pp)
			if page == nil {
				page = []mail.Message{}
			}
			return &MailListOutput{
				Index:     index,
				CacheAgeS: cacheAge,
				Body:      MailListBody{Items: page, Total: total, NextCursor: nextCursor},
			}, nil
		}

		providers := s.state.MailProviders()
		var allMsgs []mail.Message
		var partialErrs []string
		partialStoreSlow := false
		for _, res := range withMailProviderReadDeadline(ctx, providers, func(provider mail.Provider) ([]mail.Message, error) {
			return mailInboxForRecipients(provider, agents)
		}) {
			if res.err != nil {
				var timeoutErr *mailReadTimeoutError
				partialStoreSlow = partialStoreSlow || errors.As(res.err, &timeoutErr)
				partialErrs = append(partialErrs, "mail provider "+res.name+": "+res.err.Error())
				continue
			}
			allMsgs = append(allMsgs, tagRig(res.value, res.name)...)
		}
		if len(partialErrs) == len(providers) && len(providers) > 0 {
			return nil, allMailProvidersFailedError(partialErrs, partialStoreSlow)
		}
		if allMsgs == nil {
			allMsgs = []mail.Message{}
		}
		partial := len(partialErrs) > 0
		if !pp.IsPaging {
			total := len(allMsgs)
			if pp.Limit < len(allMsgs) {
				allMsgs = allMsgs[:pp.Limit]
			}
			return &MailListOutput{
				Index:     index,
				CacheAgeS: cacheAge,
				Body:      MailListBody{Items: allMsgs, Total: total, Partial: partial, PartialErrors: partialErrs},
			}, nil
		}
		page, total, nextCursor := paginate(allMsgs, pp)
		if page == nil {
			page = []mail.Message{}
		}
		return &MailListOutput{
			Index:     index,
			CacheAgeS: cacheAge,
			Body:      MailListBody{Items: page, Total: total, NextCursor: nextCursor, Partial: partial, PartialErrors: partialErrs},
		}, nil

	case "all":
		if rig != "" {
			mp := s.state.MailProvider(rig)
			if mp == nil {
				return &MailListOutput{
					Index:     index,
					CacheAgeS: cacheAge,
					Body:      MailListBody{Items: []mail.Message{}, Total: 0},
				}, nil
			}
			msgs, err := withMailReadDeadline(ctx, func() ([]mail.Message, error) {
				return mailAllForRecipients(mp, agents)
			})
			if err != nil {
				return nil, mailReadAPIError(err)
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
				return &MailListOutput{
					Index:     index,
					CacheAgeS: cacheAge,
					Body:      MailListBody{Items: msgs, Total: total},
				}, nil
			}
			page, total, nextCursor := paginate(msgs, pp)
			if page == nil {
				page = []mail.Message{}
			}
			return &MailListOutput{
				Index:     index,
				CacheAgeS: cacheAge,
				Body:      MailListBody{Items: page, Total: total, NextCursor: nextCursor},
			}, nil
		}

		providers := s.state.MailProviders()
		var allMsgs []mail.Message
		var partialErrs []string
		partialStoreSlow := false
		for _, res := range withMailProviderReadDeadline(ctx, providers, func(provider mail.Provider) ([]mail.Message, error) {
			return mailAllForRecipients(provider, agents)
		}) {
			if res.err != nil {
				var timeoutErr *mailReadTimeoutError
				partialStoreSlow = partialStoreSlow || errors.As(res.err, &timeoutErr)
				partialErrs = append(partialErrs, "mail provider "+res.name+": "+res.err.Error())
				continue
			}
			allMsgs = append(allMsgs, tagRig(res.value, res.name)...)
		}
		if len(partialErrs) == len(providers) && len(providers) > 0 {
			return nil, allMailProvidersFailedError(partialErrs, partialStoreSlow)
		}
		if allMsgs == nil {
			allMsgs = []mail.Message{}
		}
		partial := len(partialErrs) > 0
		if !pp.IsPaging {
			total := len(allMsgs)
			if pp.Limit < len(allMsgs) {
				allMsgs = allMsgs[:pp.Limit]
			}
			return &MailListOutput{
				Index:     index,
				CacheAgeS: cacheAge,
				Body:      MailListBody{Items: allMsgs, Total: total, Partial: partial, PartialErrors: partialErrs},
			}, nil
		}
		page, total, nextCursor := paginate(allMsgs, pp)
		if page == nil {
			page = []mail.Message{}
		}
		return &MailListOutput{
			Index:     index,
			CacheAgeS: cacheAge,
			Body:      MailListBody{Items: page, Total: total, NextCursor: nextCursor, Partial: partial, PartialErrors: partialErrs},
		}, nil

	default:
		return nil, huma.Error400BadRequest("unsupported status filter: " + status + "; supported: unread, all")
	}
}

// humaHandleMailGet is the Huma-typed handler for GET /v0/mail/{id}.
func (s *Server) humaHandleMailGet(ctx context.Context, input *MailGetInput) (*IndexOutput[mail.Message], error) {
	cityStore := s.state.CityBeadStore()
	if err := cacheLiveOr503(cityStore); err != nil {
		return nil, err
	}
	id := input.ID
	rig := input.Rig
	result, err := withMailReadDeadline(ctx, func() (mailGetResult, error) {
		mp, resolvedRig, err := s.findMailProviderForMessage(id, rig)
		if err != nil {
			return mailGetResult{}, err
		}
		if mp == nil {
			return mailGetResult{}, nil
		}
		msg, err := mp.Get(id)
		if err != nil {
			return mailGetResult{}, err
		}
		return mailGetResult{Message: msg, Rig: resolvedRig, Found: true}, nil
	})
	if err != nil {
		if errors.Is(err, mail.ErrNotFound) {
			return nil, huma.Error404NotFound(err.Error())
		}
		return nil, mailReadAPIError(err)
	}
	if !result.Found {
		return nil, huma.Error404NotFound("message " + id + " not found")
	}
	result.Message.Rig = result.Rig
	return &IndexOutput[mail.Message]{
		Index:     s.latestIndex(),
		CacheAgeS: cacheAgeSeconds(cityStore),
		Body:      result.Message,
	}, nil
}

// humaHandleMailSend is the Huma-typed handler for POST /v0/mail.
// Body validation (To and Subject required, minLength:"1") is enforced by
// the framework from MailSendInput's struct tags.
func (s *Server) humaHandleMailSend(ctx context.Context, input *MailSendInput) (*IndexOutput[mail.Message], error) {
	resolved, resolveErr := s.resolveMailSendRecipientWithContext(ctx, input.Body.To)
	if resolveErr != nil {
		if errors.Is(resolveErr, errMailNoBeadStore) {
			return nil, huma.Error400BadRequest(resolveErr.Error())
		}
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
	telemetry.RecordMailOp(ctx, "send", err)
	if err != nil {
		s.idem.unreserve(idemKey)
		return nil, huma.Error500InternalServerError(err.Error())
	}
	msg.Rig = input.Body.Rig
	s.idem.storeResponse(idemKey, bodyHash, msg)
	s.recordMailEvent(events.MailSent, msg.From, msg.ID, input.Body.Rig, &msg)

	return &IndexOutput[mail.Message]{
		Index: s.latestIndex(),
		Body:  msg,
	}, nil
}

// humaHandleMailCount is the Huma-typed handler for GET /v0/mail/count.
func (s *Server) humaHandleMailCount(ctx context.Context, input *MailCountInput) (*MailCountOutput, error) {
	cityStore := s.state.CityBeadStore()
	if err := cacheLiveOr503(cityStore); err != nil {
		return nil, err
	}
	agents := s.resolveMailQueryRecipientsWithContext(ctx, input.Agent)
	rig := input.Rig
	cacheAge := cacheAgeSeconds(cityStore)

	if rig != "" {
		mp := s.state.MailProvider(rig)
		if mp == nil {
			resp := &MailCountOutput{CacheAgeS: cacheAge}
			resp.Body.Total = 0
			resp.Body.Unread = 0
			return resp, nil
		}
		counts, err := withMailReadDeadline(ctx, func() (mailReadCounts, error) {
			total, unread, err := mailCountForRecipients(mp, agents)
			return mailReadCounts{Total: total, Unread: unread}, err
		})
		if err != nil {
			return nil, mailReadAPIError(err)
		}
		resp := &MailCountOutput{CacheAgeS: cacheAge}
		resp.Body.Total = counts.Total
		resp.Body.Unread = counts.Unread
		return resp, nil
	}

	// Aggregate across all rigs (deduplicated by provider identity).
	// Fail-open: one bad provider turns into partial_errors, 503 only
	// when every provider fails — matches humaHandleMailList.
	providers := s.state.MailProviders()
	var totalAll, unreadAll int
	var partialErrs []string
	partialStoreSlow := false
	for _, res := range withMailProviderReadDeadline(ctx, providers, func(provider mail.Provider) (mailReadCounts, error) {
		total, unread, err := mailCountForRecipients(provider, agents)
		return mailReadCounts{Total: total, Unread: unread}, err
	}) {
		if res.err != nil {
			var timeoutErr *mailReadTimeoutError
			partialStoreSlow = partialStoreSlow || errors.As(res.err, &timeoutErr)
			partialErrs = append(partialErrs, "mail provider "+res.name+": "+res.err.Error())
			continue
		}
		totalAll += res.value.Total
		unreadAll += res.value.Unread
	}
	if len(partialErrs) == len(providers) && len(providers) > 0 {
		return nil, allMailProvidersFailedError(partialErrs, partialStoreSlow)
	}
	resp := &MailCountOutput{CacheAgeS: cacheAge}
	resp.Body.Total = totalAll
	resp.Body.Unread = unreadAll
	resp.Body.Partial = len(partialErrs) > 0
	resp.Body.PartialErrors = partialErrs
	return resp, nil
}

// humaHandleMailThread is the Huma-typed handler for GET /v0/mail/thread/{id}.
func (s *Server) humaHandleMailThread(ctx context.Context, input *MailThreadInput) (*MailListOutput, error) {
	threadID := input.ID
	rig := input.Rig
	index := s.latestIndex()

	if rig != "" {
		mp := s.state.MailProvider(rig)
		if mp == nil {
			return nil, huma.Error404NotFound("rig " + rig + " not found")
		}
		msgs, err := withMailReadDeadline(ctx, func() ([]mail.Message, error) {
			return mp.Thread(threadID)
		})
		if err != nil {
			return nil, mailReadAPIError(err)
		}
		if msgs == nil {
			msgs = []mail.Message{}
		}
		msgs = tagRig(msgs, rig)
		return &MailListOutput{
			Index: index,
			Body:  MailListBody{Items: msgs, Total: len(msgs)},
		}, nil
	}

	// Aggregate thread messages across all providers.
	// Fail-open: one bad provider returns partial+errors, 503 only when
	// every provider fails — matches humaHandleMailList.
	providers := s.state.MailProviders()
	var allMsgs []mail.Message
	var partialErrs []string
	partialStoreSlow := false
	for _, res := range withMailProviderReadDeadline(ctx, providers, func(provider mail.Provider) ([]mail.Message, error) {
		return provider.Thread(threadID)
	}) {
		if res.err != nil {
			var timeoutErr *mailReadTimeoutError
			partialStoreSlow = partialStoreSlow || errors.As(res.err, &timeoutErr)
			partialErrs = append(partialErrs, "mail provider "+res.name+": "+res.err.Error())
			continue
		}
		allMsgs = append(allMsgs, tagRig(res.value, res.name)...)
	}
	if len(partialErrs) == len(providers) && len(providers) > 0 {
		return nil, allMailProvidersFailedError(partialErrs, partialStoreSlow)
	}
	if allMsgs == nil {
		allMsgs = []mail.Message{}
	}
	return &MailListOutput{
		Index: index,
		Body:  MailListBody{Items: allMsgs, Total: len(allMsgs), Partial: len(partialErrs) > 0, PartialErrors: partialErrs},
	}, nil
}

// humaHandleMailRead is the Huma-typed handler for POST /v0/mail/{id}/read.
func (s *Server) humaHandleMailRead(ctx context.Context, input *MailReadInput) (*OKResponse, error) {
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
		telemetry.RecordMailOp(ctx, "mark_read", err)
		return nil, huma.Error500InternalServerError(err.Error())
	}
	telemetry.RecordMailOp(ctx, "mark_read", nil)
	if err := waitForMailReadState(ctx, mp, id, true); err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	s.recordMailEvent(events.MailMarkedRead, "api", id, resolvedRig, nil)
	resp := &OKResponse{}
	resp.Body.Status = "read"
	return resp, nil
}

// humaHandleMailMarkUnread is the Huma-typed handler for POST /v0/mail/{id}/mark-unread.
func (s *Server) humaHandleMailMarkUnread(ctx context.Context, input *MailMarkUnreadInput) (*OKResponse, error) {
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
		telemetry.RecordMailOp(ctx, "mark_unread", err)
		return nil, huma.Error500InternalServerError(err.Error())
	}
	telemetry.RecordMailOp(ctx, "mark_unread", nil)
	if err := waitForMailReadState(ctx, mp, id, false); err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	s.recordMailEvent(events.MailMarkedUnread, "api", id, resolvedRig, nil)
	resp := &OKResponse{}
	resp.Body.Status = "unread"
	return resp, nil
}

func waitForMailReadState(ctx context.Context, mp mail.Provider, id string, want bool) error {
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()

	for {
		msg, err := mp.Get(id)
		if err != nil {
			return err
		}
		if msg.Read == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("mail read state did not become visible")
		case <-tick.C:
		}
	}
}

// humaHandleMailArchive is the Huma-typed handler for POST /v0/mail/{id}/archive.
func (s *Server) humaHandleMailArchive(ctx context.Context, input *MailArchiveInput) (*OKResponse, error) {
	id := input.ID
	rig := input.Rig
	mp, resolvedRig, err := s.findMailProviderForMessage(id, rig)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	if mp == nil {
		// Idempotent: archive removes the bead, so a repeat call finds no
		// owning provider. Matches mail.ErrAlreadyArchived at the CLI/provider layer.
		resp := &OKResponse{}
		resp.Body.Status = "archived"
		return resp, nil
	}
	if err := mp.Archive(id); err != nil {
		if errors.Is(err, mail.ErrAlreadyArchived) {
			resp := &OKResponse{}
			resp.Body.Status = "archived"
			return resp, nil
		}
		telemetry.RecordMailOp(ctx, "archive", err)
		return nil, huma.Error500InternalServerError(err.Error())
	}
	telemetry.RecordMailOp(ctx, "archive", nil)
	s.recordMailEvent(events.MailArchived, "api", id, resolvedRig, nil)
	resp := &OKResponse{}
	resp.Body.Status = "archived"
	return resp, nil
}

// humaHandleMailReply is the Huma-typed handler for POST /v0/mail/{id}/reply.
func (s *Server) humaHandleMailReply(ctx context.Context, input *MailReplyInput) (*IndexOutput[mail.Message], error) {
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
	telemetry.RecordMailOp(ctx, "reply", err)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	msg.Rig = resolvedRig
	s.recordMailEvent(events.MailReplied, msg.From, msg.ID, resolvedRig, &msg)

	return &IndexOutput[mail.Message]{
		Index: s.latestIndex(),
		Body:  msg,
	}, nil
}

// humaHandleMailDelete is the Huma-typed handler for DELETE /v0/mail/{id}.
func (s *Server) humaHandleMailDelete(ctx context.Context, input *MailDeleteInput) (*OKResponse, error) {
	id := input.ID
	rig := input.Rig
	mp, resolvedRig, err := s.findMailProviderForMessage(id, rig)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	if mp == nil {
		// Idempotent: delete removes the bead, so a repeat call finds no
		// owning provider. Matches mail.ErrAlreadyArchived at the CLI/provider layer.
		resp := &OKResponse{}
		resp.Body.Status = "deleted"
		return resp, nil
	}
	if err := mp.Delete(id); err != nil {
		if errors.Is(err, mail.ErrAlreadyArchived) {
			resp := &OKResponse{}
			resp.Body.Status = "deleted"
			return resp, nil
		}
		telemetry.RecordMailOp(ctx, "delete", err)
		return nil, huma.Error500InternalServerError(err.Error())
	}
	telemetry.RecordMailOp(ctx, "delete", nil)
	s.recordMailEvent(events.MailDeleted, "api", id, resolvedRig, nil)
	resp := &OKResponse{}
	resp.Body.Status = "deleted"
	return resp, nil
}
