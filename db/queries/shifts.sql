-- Shifts and shift groups (PJ48 m_shifts, m_shift_group_master, t_shift_group_detail).
-- Everything runs inside database.WithTenant. Deleted rows are hidden from every read here,
-- but schedules made with them keep pointing at them.

-- name: ListShifts :many
SELECT * FROM shifts
WHERE deleted_at IS NULL
  AND (sqlc.narg('active')::boolean IS NULL OR active = sqlc.narg('active'))
  AND (sqlc.narg('q')::text IS NULL OR name ILIKE '%' || sqlc.narg('q') || '%')
ORDER BY lower(name), id
LIMIT @page_limit OFFSET @page_offset;

-- name: CountShifts :one
SELECT count(*) FROM shifts
WHERE deleted_at IS NULL
  AND (sqlc.narg('active')::boolean IS NULL OR active = sqlc.narg('active'))
  AND (sqlc.narg('q')::text IS NULL OR name ILIKE '%' || sqlc.narg('q') || '%');

-- name: GetShift :one
SELECT * FROM shifts WHERE id = $1 AND deleted_at IS NULL;

-- name: GetShiftForUpdate :one
SELECT * FROM shifts WHERE id = $1 AND deleted_at IS NULL FOR UPDATE;

-- name: GetShiftsByIDs :many
SELECT * FROM shifts WHERE id = ANY(@ids::bigint[]) AND deleted_at IS NULL;

-- name: CreateShift :one
INSERT INTO shifts (name, start_minute, end_minute, is_off, tolerance_minutes, active, created_by, updated_by, created_at, updated_at)
VALUES (@name, @start_minute, @end_minute, @is_off, @tolerance_minutes, @active, @actor, @actor, @now, @now)
RETURNING *;

-- is_off is not here: an OFF day cannot become a working shift (or the reverse) under the
-- schedules that already use it.
-- name: UpdateShift :one
UPDATE shifts SET
    name              = COALESCE(sqlc.narg('name'), name),
    start_minute      = COALESCE(sqlc.narg('start_minute'), start_minute),
    end_minute        = COALESCE(sqlc.narg('end_minute'), end_minute),
    tolerance_minutes = COALESCE(sqlc.narg('tolerance_minutes'), tolerance_minutes),
    active            = COALESCE(sqlc.narg('active'), active),
    updated_by        = @actor,
    updated_at        = @now
WHERE id = @id AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteShift :execrows
UPDATE shifts SET deleted_at = @now, active = false, updated_by = @actor, updated_at = @now
WHERE id = @id AND deleted_at IS NULL;

-- A shift a group still uses cannot be deleted: the group would generate days without a shift.
-- name: CountGroupDaysUsingShift :one
SELECT count(*) FROM shift_group_days d
JOIN shift_groups g ON g.id = d.shift_group_id
WHERE d.shift_id = $1 AND g.deleted_at IS NULL;

-- Nor one that is scheduled from a date on: the schedule would point at a shift nobody can see.
-- name: CountSchedulesOfShiftFrom :one
SELECT count(*) FROM schedules WHERE shift_id = @shift_id AND work_date >= @from_date;

-- name: ListShiftGroups :many
SELECT * FROM shift_groups
WHERE deleted_at IS NULL
  AND (sqlc.narg('q')::text IS NULL OR name ILIKE '%' || sqlc.narg('q') || '%')
ORDER BY lower(name), id
LIMIT @page_limit OFFSET @page_offset;

-- name: CountShiftGroups :one
SELECT count(*) FROM shift_groups
WHERE deleted_at IS NULL
  AND (sqlc.narg('q')::text IS NULL OR name ILIKE '%' || sqlc.narg('q') || '%');

-- name: GetShiftGroup :one
SELECT * FROM shift_groups WHERE id = $1 AND deleted_at IS NULL;

-- name: GetShiftGroupForUpdate :one
SELECT * FROM shift_groups WHERE id = $1 AND deleted_at IS NULL FOR UPDATE;

-- The days of groups, Monday first, with their shift.
-- name: ListShiftGroupDays :many
SELECT d.shift_group_id, d.weekday, s.id AS shift_id, s.name AS shift_name, s.start_minute, s.end_minute, s.is_off
FROM shift_group_days d
JOIN shifts s ON s.id = d.shift_id
WHERE d.shift_group_id = ANY(@group_ids::bigint[])
ORDER BY d.shift_group_id, d.weekday;

-- name: CreateShiftGroup :one
INSERT INTO shift_groups (name, created_by, updated_by, created_at, updated_at)
VALUES (@name, @actor, @actor, @now, @now)
RETURNING *;

-- name: UpsertShiftGroupDay :exec
INSERT INTO shift_group_days (shift_group_id, weekday, shift_id)
VALUES (@shift_group_id, @weekday, @shift_id)
ON CONFLICT (shift_group_id, weekday) DO UPDATE SET shift_id = EXCLUDED.shift_id;

-- name: UpdateShiftGroup :one
UPDATE shift_groups SET
    name       = COALESCE(sqlc.narg('name'), name),
    updated_by = @actor,
    updated_at = @now
WHERE id = @id AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteShiftGroup :execrows
UPDATE shift_groups SET deleted_at = @now, updated_by = @actor, updated_at = @now
WHERE id = @id AND deleted_at IS NULL;

-- A group a person is assigned to cannot be deleted: their schedule is generated from it.
-- name: CountAssignmentsOfShiftGroup :one
SELECT count(*) FROM personnel_shift_assignments WHERE shift_group_id = $1;

-- name: GetShiftByName :one
SELECT * FROM shifts WHERE lower(name) = lower(@name) AND deleted_at IS NULL;

-- name: GetShiftGroupByName :one
SELECT * FROM shift_groups WHERE lower(name) = lower(@name) AND deleted_at IS NULL;
