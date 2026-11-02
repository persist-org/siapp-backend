-- The in-app inbox (Phase 05.1). Everything runs inside database.WithTenant, so row-level
-- security keeps another organization's notifications invisible. Every read and write below is
-- ALSO limited to one recipient: a person sees and changes only their own rows, and a row that
-- is someone else's is simply not there (the same answer as one that does not exist).

-- One statement writes a notification for every recipient and returns the rows.
--
-- ⚠ Not :copyfrom, although the plan names it: COPY cannot RETURN the new ids, and the dispatcher
-- (push, realtime) needs them. unnest()/the personnels join is one statement and one round trip
-- all the same.
--
-- The recipients come from the personnels table, not straight from the array. Under row-level
-- security that table shows only this organization's people, so an id of another organization (a
-- bug in a caller) or of nobody gets no row instead of a notification in the wrong tenant, and
-- one stale id cannot fail the batch for everyone else: a panic must reach the rest of the regu.
-- The caller compares the row count with the number of ids it sent and logs the difference.
-- name: InsertNotifications :many
INSERT INTO notifications (recipient_personnel_id, type, title, body, entity_type, entity_id,
                           data, priority, created_at, updated_at)
SELECT p.id, @type::notification_type, @title::text, @body::text,
       sqlc.narg('entity_type')::text, sqlc.narg('entity_id')::bigint,
       @data::jsonb, @priority::notification_priority, @now::timestamptz, @now::timestamptz
FROM personnels p
WHERE p.id = ANY(@recipient_ids::bigint[])
ORDER BY p.id
RETURNING *;

-- name: GetNotification :one
SELECT * FROM notifications
WHERE id = @id AND recipient_personnel_id = @recipient_personnel_id AND deleted_at IS NULL;

-- The plain list: newest first. The id breaks a tie between rows one Notify wrote together.
-- name: ListNotifications :many
SELECT * FROM notifications
WHERE recipient_personnel_id = @recipient_personnel_id AND deleted_at IS NULL
  AND (NOT @unread_only::boolean OR read_at IS NULL)
ORDER BY created_at DESC, id DESC
LIMIT @page_limit OFFSET @page_offset;

-- The counter, and the total of the plain list.
-- name: CountNotifications :one
SELECT (count(*) FILTER (WHERE read_at IS NULL))::int AS unread, count(*)::int AS total
FROM notifications
WHERE recipient_personnel_id = @recipient_personnel_id AND deleted_at IS NULL;

-- The delta pull: every row that changed after (updated_at, id), oldest change first, DELETED
-- ROWS INCLUDED. Their deleted_at turns them into tombstones in Go. One ordered stream means one
-- keyset cursor and one LIMIT for rows and tombstones together. The caller asks for one row more
-- than it will return, to learn whether there is another page.
-- name: ListNotificationChanges :many
SELECT * FROM notifications
WHERE recipient_personnel_id = @recipient_personnel_id
  AND (updated_at, id) > (@after_updated_at::timestamptz, @after_id::bigint)
ORDER BY updated_at, id
LIMIT @page_limit;

-- Unread to read. The conditions are in the statement: a second call finds no unread row, changes
-- nothing (updated_at stays, so a replay is not a change for the delta sync) and the caller
-- re-reads the row.
-- name: MarkNotificationRead :one
UPDATE notifications SET read_at = @now, updated_at = @now
WHERE id = @id AND recipient_personnel_id = @recipient_personnel_id
  AND deleted_at IS NULL AND read_at IS NULL
RETURNING *;

-- name: MarkAllNotificationsRead :execrows
UPDATE notifications SET read_at = @now, updated_at = @now
WHERE recipient_personnel_id = @recipient_personnel_id AND deleted_at IS NULL AND read_at IS NULL;
