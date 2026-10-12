// Package audit records who did what to which entity, for sensitive writes.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"siapp/internal/db"
)

// Entry describes one audited action.
type Entry struct {
	// ActorID is the user who acted. Leave it empty for system actions (jobs, seeds).
	ActorID string
	// Action is a dotted, module-first verb, e.g. "attendance.absence.create".
	Action     string
	EntityType string
	EntityID   string
	// Metadata is marshalled to JSON. Put in what is needed to understand the change,
	// never passwords, tokens or hashes. Nil stores NULL.
	Metadata any
	// At is the time of the action. Services pass clock.Now() so tests control it; zero
	// means "use the database clock".
	At time.Time
}

// Record writes an audit row.
//
// ⚠ q must be the *db.Queries of the transaction that makes the change (the one handed to
// the database.WithTx callback), not the pool's. The audit row then commits or rolls back
// together with the change: no change goes unrecorded, and no rolled-back change leaves a
// row claiming it happened.
func Record(ctx context.Context, q *db.Queries, e Entry) error {
	if e.Action == "" || e.EntityType == "" || e.EntityID == "" {
		return errors.New("record audit: action, entity type and entity ID are required")
	}

	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generate audit id: %w", err)
	}

	var metadata json.RawMessage
	if e.Metadata != nil {
		if metadata, err = json.Marshal(e.Metadata); err != nil {
			return fmt.Errorf("marshal audit metadata: %w", err)
		}
	}

	err = q.InsertAuditLog(ctx, db.InsertAuditLogParams{
		ID:         id.String(),
		ActorID:    sql.NullString{String: e.ActorID, Valid: e.ActorID != ""},
		Action:     e.Action,
		EntityType: e.EntityType,
		EntityID:   e.EntityID,
		Metadata:   metadata,
		CreatedAt:  sql.NullTime{Time: e.At.UTC(), Valid: !e.At.IsZero()},
	})
	if err != nil {
		return fmt.Errorf("insert audit log: %w", err)
	}
	return nil
}
