package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"

	"siapp/internal/db"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
)

// Notify writes a notification to the inbox of each recipient and hands the rows to the
// Dispatcher (push, realtime). The organization is the one of the authenticated caller in ctx
// (auth.From); a caller with no request behind it, such as a worker, puts one there with
// auth.WithPrincipal.
//
// Call it AFTER the transaction of the business action has committed, never inside it: the rows
// then exist only for an action that happened, and nothing here can hold the action's locks.
//
// ⚠ It returns nothing, on purpose. A notification failure must never fail the business action
// that caused it: the leave is granted, the panic is recorded, the person moved, and a full
// disk or a dead Redis in the notification path is no reason to tell the user it failed (and
// have them retry what already worked). Every failure is logged and swallowed, a panic
// included, because the HTTP recoverer would turn one into a 500 for an action that succeeded.
//
// It also runs on a context that is no longer cancelled by the request: the phone that pressed
// the panic button often loses its signal the moment the request is accepted, and a cancelled
// context would then drop the very notification the team is waiting for. notifyTimeout bounds it
// instead.
func (s *Service) Notify(ctx context.Context, in Input) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.ErrorContext(ctx, "notifications: Notify panicked",
				slog.Any("panic", r), slog.String("stack", string(debug.Stack())))
		}
	}()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notifyTimeout)
	defer cancel()

	if err := s.notify(ctx, in); err != nil {
		s.logger.ErrorContext(ctx, "notifications: could not notify",
			slog.String("type", string(in.Type)), slog.Any("error", err))
	}
}

func (s *Service) notify(ctx context.Context, in Input) error {
	p, ok := auth.From(ctx)
	if !ok {
		return errors.New("no organization in the context: Notify needs an authenticated caller")
	}
	if in.Entity != nil && (in.Entity.Type == "" || in.Entity.ID < 1) {
		return errors.New("entity needs a type and a positive id")
	}

	recipients := recipientsOf(in.RecipientIDs, in.ExcludePersonnelID)
	if len(recipients) == 0 {
		return nil
	}

	data := in.Data
	if data == nil {
		data = map[string]any{}
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode data: %w", err)
	}
	priority := in.Priority
	if priority == "" {
		priority = PriorityNormal
	}
	title, body := Render(in.Type, in.Vars)

	var sent []Notification
	err = database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) (err error) {
		sent, err = s.repo.insert(ctx, q, insertParams{
			Type: in.Type, Title: title, Body: body, Entity: in.Entity, Data: encoded,
			Priority: priority, RecipientIDs: recipients, Now: s.clock.Now(),
		})
		return err
	})
	if err != nil {
		return err
	}
	// The query keeps only people of this organization. A shortfall is a caller bug (a stale id,
	// or an id from another tenant): the rest were notified, and this says it happened.
	if len(sent) < len(recipients) {
		s.logger.WarnContext(ctx, "notifications: some recipients are not people of the organization",
			slog.String("type", string(in.Type)),
			slog.Int("requested", len(recipients)), slog.Int("written", len(sent)))
	}
	if len(sent) == 0 {
		return nil
	}

	if err := s.dispatcher.Dispatch(ctx, p.OrganizationID, sent); err != nil {
		return fmt.Errorf("dispatch: %w", err)
	}
	return nil
}

// recipientsOf returns ids without duplicates, in the order given, leaving out the excluded
// person and any id that cannot be a personnels id.
func recipientsOf(ids []int64, exclude int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id < 1 || id == exclude {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
