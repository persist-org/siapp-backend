package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"siapp/internal/db"
)

// repo wraps the notification queries. Every method receives the *db.Queries of a tenant
// transaction (database.WithTenant); nothing here opens one. Every method that reads or changes
// a row takes the recipient: a person's inbox is theirs alone.
type repo struct{}

func timestamptz(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func optTime(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func toNotification(n db.Notification) (Notification, error) {
	data := map[string]any{}
	if len(n.Data) > 0 {
		if err := json.Unmarshal(n.Data, &data); err != nil {
			return Notification{}, fmt.Errorf("decode data of notification %d: %w", n.ID, err)
		}
	}
	out := Notification{
		ID:             n.ID,
		OrganizationID: n.OrganizationID,
		RecipientID:    n.RecipientPersonnelID,
		Type:           n.Type,
		Title:          n.Title,
		Body:           n.Body,
		Data:           data,
		Priority:       n.Priority,
		ReadAt:         optTime(n.ReadAt),
		CreatedAt:      n.CreatedAt.Time,
		UpdatedAt:      n.UpdatedAt.Time,
		DeletedAt:      optTime(n.DeletedAt),
	}
	if n.EntityType.Valid {
		out.EntityType = &n.EntityType.String
	}
	if n.EntityID.Valid {
		out.EntityID = &n.EntityID.Int64
	}
	return out, nil
}

func toNotifications(rows []db.Notification) ([]Notification, error) {
	out := make([]Notification, len(rows))
	for i, row := range rows {
		n, err := toNotification(row)
		if err != nil {
			return nil, err
		}
		out[i] = n
	}
	return out, nil
}

// insertParams is what a batch of new rows needs; the organization comes from the tenant setting.
type insertParams struct {
	Type         Type
	Title        string
	Body         string
	Entity       *Entity
	Data         json.RawMessage
	Priority     Priority
	RecipientIDs []int64
	Now          time.Time
}

// insert writes one row per recipient that is a person of the tenant, and returns the rows. A
// recipient that is not one (another organization's, or nobody's) gets none, so the result can
// be shorter than RecipientIDs.
func (repo) insert(ctx context.Context, q *db.Queries, p insertParams) ([]Notification, error) {
	arg := db.InsertNotificationsParams{
		Type: p.Type, Title: p.Title, Body: p.Body, Data: p.Data, Priority: p.Priority,
		Now: timestamptz(p.Now), RecipientIds: p.RecipientIDs,
	}
	if p.Entity != nil {
		arg.EntityType = pgtype.Text{String: p.Entity.Type, Valid: true}
		arg.EntityID = pgtype.Int8{Int64: p.Entity.ID, Valid: true}
	}
	rows, err := q.InsertNotifications(ctx, arg)
	if err != nil {
		return nil, fmt.Errorf("insert notifications: %w", err)
	}
	return toNotifications(rows)
}

// get returns ErrNotFound for an id with no visible row of this recipient: it does not exist,
// it is someone else's, or row-level security hides it because it belongs to another
// organization.
func (repo) get(ctx context.Context, q *db.Queries, id, recipient int64) (Notification, error) {
	row, err := q.GetNotification(ctx, db.GetNotificationParams{ID: id, RecipientPersonnelID: recipient})
	if errors.Is(err, pgx.ErrNoRows) {
		return Notification{}, ErrNotFound
	}
	if err != nil {
		return Notification{}, fmt.Errorf("get notification: %w", err)
	}
	return toNotification(row)
}

func (repo) list(ctx context.Context, q *db.Queries, recipient int64, in ListInput) ([]Notification, error) {
	rows, err := q.ListNotifications(ctx, db.ListNotificationsParams{
		RecipientPersonnelID: recipient, UnreadOnly: in.UnreadOnly,
		PageLimit: int32(in.Limit), PageOffset: int32(in.Offset), //nolint:gosec // the handler bounds both (httpx.MaxLimit, maxPage)
	})
	if err != nil {
		return nil, fmt.Errorf("list notifications: %w", err)
	}
	return toNotifications(rows)
}

func (repo) count(ctx context.Context, q *db.Queries, recipient int64) (Count, error) {
	row, err := q.CountNotifications(ctx, recipient)
	if err != nil {
		return Count{}, fmt.Errorf("count notifications: %w", err)
	}
	return Count{Unread: int(row.Unread), Total: int(row.Total)}, nil
}

// changesAfter returns up to limit rows, deleted ones included, that come after the cursor in
// (updated_at, id) order.
func (repo) changesAfter(ctx context.Context, q *db.Queries, recipient int64, after Cursor, limit int) ([]Notification, error) {
	rows, err := q.ListNotificationChanges(ctx, db.ListNotificationChangesParams{
		RecipientPersonnelID: recipient,
		AfterUpdatedAt:       timestamptz(after.UpdatedAt),
		AfterID:              after.ID,
		PageLimit:            int32(limit), //nolint:gosec // the handler bounds it (httpx.MaxLimit)
	})
	if err != nil {
		return nil, fmt.Errorf("list notification changes: %w", err)
	}
	return toNotifications(rows)
}

// markRead moves an unread notification of this recipient to read. updated is false when there
// was nothing to move: it is already read, or it is not theirs, or it does not exist.
func (repo) markRead(ctx context.Context, q *db.Queries, id, recipient int64, now time.Time) (n Notification, updated bool, err error) {
	row, err := q.MarkNotificationRead(ctx, db.MarkNotificationReadParams{ID: id, RecipientPersonnelID: recipient, Now: timestamptz(now)})
	if errors.Is(err, pgx.ErrNoRows) {
		return Notification{}, false, nil
	}
	if err != nil {
		return Notification{}, false, fmt.Errorf("mark notification read: %w", err)
	}
	n, err = toNotification(row)
	return n, err == nil, err
}

func (repo) markAllRead(ctx context.Context, q *db.Queries, recipient int64, now time.Time) error {
	if _, err := q.MarkAllNotificationsRead(ctx, db.MarkAllNotificationsReadParams{RecipientPersonnelID: recipient, Now: timestamptz(now)}); err != nil {
		return fmt.Errorf("mark all notifications read: %w", err)
	}
	return nil
}
