// Package audit records sensitive writes in the append-only audit_logs table.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/platform/clock"
)

// TaskEnsurePartitions is the name of the monthly worker task that calls EnsurePartitions.
// The worker (Phase 01.2) registers it.
const TaskEnsurePartitions = "audit:partitions"

// partitionsAhead is how many months beyond the current one EnsurePartitions keeps ready.
// The migration creates the current month and the next two; the monthly task keeps that
// window moving, so even a few missed runs leave months of room.
const partitionsAhead = 3

// Entry is one audit record.
type Entry struct {
	// ActorPersonnelID is who did it; nil for the system (a scheduled job).
	ActorPersonnelID *int64
	// Action is "<module>.<entity>.<verb>", e.g. "attendance.absence.create".
	Action     string
	EntityType string
	EntityID   int64
	// Metadata is context for a reviewer: the old and new status, the reason given. It
	// must never hold a token, a password, a NIK or a camera URL; Record rejects keys that
	// look like those, but the values are the caller's responsibility.
	Metadata map[string]any
}

// ErrSensitiveMetadata is returned by Record when Metadata has a key that is reserved for
// secrets or personal data. It is a programming error, not a client error.
var ErrSensitiveMetadata = errors.New("audit: metadata must not contain secrets or personal identifiers")

// Record writes e through q, which must be the Queries of the tenant transaction that makes
// the change (the q passed to the WithTenant callback). The audit row then commits or
// rolls back together with the change: there is never a change without its audit row, and
// never an audit row for a change that did not happen.
func Record(ctx context.Context, q *db.Queries, e Entry) error {
	if e.Action == "" || e.EntityType == "" {
		return errors.New("audit: Action and EntityType are required")
	}
	if key, bad := sensitiveKey(e.Metadata); bad {
		return fmt.Errorf("%w (key %q)", ErrSensitiveMetadata, key)
	}
	metadata := []byte("{}")
	if len(e.Metadata) > 0 {
		var err error
		if metadata, err = json.Marshal(e.Metadata); err != nil {
			return fmt.Errorf("audit: encode metadata: %w", err)
		}
	}
	actor := pgtype.Int8{}
	if e.ActorPersonnelID != nil {
		actor = pgtype.Int8{Int64: *e.ActorPersonnelID, Valid: true}
	}
	if err := q.InsertAuditLog(ctx, db.InsertAuditLogParams{
		ActorPersonnelID: actor,
		Action:           e.Action,
		EntityType:       e.EntityType,
		EntityID:         e.EntityID,
		Metadata:         metadata,
	}); err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return nil
}

// EnsurePartitions makes sure the audit_logs partitions for the current month and the next
// partitionsAhead months exist. It is idempotent. It runs as siapp_app through the
// SECURITY DEFINER function ensure_audit_partition, because the app role cannot create
// tables itself.
func EnsurePartitions(ctx context.Context, pool *pgxpool.Pool, clk clock.Clock) error {
	now := clk.Now().UTC()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i <= partitionsAhead; i++ {
		month := first.AddDate(0, i, 0)
		if _, err := pool.Exec(ctx, "SELECT ensure_audit_partition($1::date)", month.Format(time.DateOnly)); err != nil {
			return fmt.Errorf("audit: ensure partition %s: %w", month.Format("2006-01"), err)
		}
	}
	return nil
}

// sensitive lists metadata keys (compared lower-case, without "_" and "-") that must not
// be stored, and the fragments that make any key suspicious.
var (
	sensitiveExact     = map[string]bool{"nik": true, "ktp": true, "authorization": true, "cookie": true}
	sensitiveFragments = []string{"password", "passwd", "token", "secret", "apikey", "cameraurl", "streamurl", "rtsp", "nationalid"}
)

// sensitiveKey reports the first key, at any depth, that looks like a secret or a NIK.
func sensitiveKey(v any) (string, bool) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if isSensitive(k) {
				return k, true
			}
			if key, bad := sensitiveKey(val); bad {
				return key, true
			}
		}
	case []any:
		for _, val := range x {
			if key, bad := sensitiveKey(val); bad {
				return key, true
			}
		}
	}
	return "", false
}

func isSensitive(key string) bool {
	k := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(key))
	if sensitiveExact[k] {
		return true
	}
	for _, f := range sensitiveFragments {
		if strings.Contains(k, f) {
			return true
		}
	}
	return false
}
