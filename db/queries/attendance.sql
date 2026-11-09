-- Attendance capture (module 07). Everything runs inside database.WithTenant.

-- The rows of a person over a range of work dates: what the today list and the check-in lookup
-- match the scheduled instances against.
-- name: ListAttendancesOfPersonnelBetween :many
SELECT * FROM attendances
WHERE personnel_id = @personnel_id AND work_date BETWEEN @date_from AND @date_to
ORDER BY work_date, id;

-- The person's latest scheduled working day before a date: the day a new check-in must find settled.
-- An OFF shift is not a working day.
-- name: GetPreviousWorkingDay :one
SELECT s.work_date
FROM schedules s
JOIN shifts sh ON sh.id = s.shift_id
WHERE s.personnel_id = @personnel_id AND s.work_date < @before AND NOT sh.is_off
ORDER BY s.work_date DESC
LIMIT 1;

-- Whether the person has ever checked in through SiAPP for a day before a date: a missed day before
-- their first check-in is not an absence SiAPP can judge (go-live, a new hire, a back-dated schedule).
-- name: HasCheckInBefore :one
SELECT EXISTS (
    SELECT 1 FROM attendances
    WHERE personnel_id = @personnel_id AND work_date < @before AND check_in_at IS NOT NULL
);

-- name: GetAttendance :one
SELECT * FROM attendances WHERE id = @id;

-- Locked, so a check-out, an "update pulang" and (later) a correction of one record take turns.
-- name: GetAttendanceForUpdate :one
SELECT * FROM attendances WHERE id = @id FOR UPDATE;

-- The record of one shift instance, locked: a check-out needs it, and two of them take turns.
-- name: GetAttendanceOfInstanceForUpdate :one
SELECT * FROM attendances
WHERE personnel_id = @personnel_id AND team_code = @team_code AND work_date = @work_date AND shift_id = @shift_id
FOR UPDATE;

-- ⚠ Insert-or-nothing instead of insert-and-catch: a unique violation would abort the whole
-- transaction, and the caller still has to read the row that is already there. With DO NOTHING a
-- second check-in that races the first waits for it to commit, then gets no row back, and the
-- caller reads the winner.
-- name: InsertAttendanceCheckIn :one
INSERT INTO attendances (
    personnel_id, team_code, project_code, work_date, shift_id, shift_starts_at, shift_ends_at, timezone,
    status, check_in_at, check_in_method, check_in_media_id, check_in_latitude, check_in_longitude,
    check_in_accuracy_meters, check_in_distance_meters, check_in_within_area, check_in_qr_danru_id,
    late_minutes, late_minutes_raw, created_at, updated_at)
VALUES (
    @personnel_id, @team_code, @project_code, @work_date, @shift_id, @shift_starts_at, @shift_ends_at, @timezone,
    'PRESENT', @now, @method, sqlc.narg('media_id'), sqlc.narg('latitude'), sqlc.narg('longitude'),
    sqlc.narg('accuracy_meters'), sqlc.narg('distance_meters'), sqlc.narg('within_area'), sqlc.narg('qr_danru_id'),
    @late_minutes::int, @late_minutes_raw::int, @now, @now)
ON CONFLICT (organization_id, personnel_id, team_code, work_date, shift_id) DO NOTHING
RETURNING *;

-- The check-in's photo is set after the insert: media.Attach needs the attendance's id, and a
-- photo id that does not exist would otherwise fail the foreign key as a 500 instead of Attach's
-- MEDIA_NOT_FOUND.
-- name: SetAttendanceCheckInMedia :exec
UPDATE attendances SET check_in_media_id = @media_id WHERE id = @id;

-- The row that is already there for an instance, without a lock: the answer to a conflicting
-- check-in only needs its status.
-- name: GetAttendanceOfInstance :one
SELECT * FROM attendances
WHERE personnel_id = @personnel_id AND team_code = @team_code AND work_date = @work_date AND shift_id = @shift_id;

-- A check-out, and "update pulang", which overwrites it: bump is 0 for the first and 1 after.
-- name: SetAttendanceCheckOut :one
UPDATE attendances SET
    check_out_at = @at, check_out_method = @method, check_out_media_id = sqlc.narg('media_id'),
    check_out_latitude = sqlc.narg('latitude'), check_out_longitude = sqlc.narg('longitude'),
    check_out_accuracy_meters = sqlc.narg('accuracy_meters'), check_out_distance_meters = sqlc.narg('distance_meters'),
    check_out_within_area = sqlc.narg('within_area'), check_out_qr_danru_id = sqlc.narg('qr_danru_id'),
    check_out_update_count = check_out_update_count + @bump::int,
    personel_sync_status = 'PENDING', updated_at = @now
WHERE id = @id
RETURNING *;

-- name: InsertPersonelOutbox :one
INSERT INTO personel_outbox (kind, aggregate_id, payload, next_attempt_at)
VALUES (@kind, @aggregate_id, @payload, @now)
RETURNING id, event_id;
