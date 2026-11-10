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
    late_minutes, late_minutes_raw, shift_swap_id, created_at, updated_at)
VALUES (
    @personnel_id, @team_code, @project_code, @work_date, @shift_id, @shift_starts_at, @shift_ends_at, @timezone,
    'PRESENT', @now, @method, sqlc.narg('media_id'), sqlc.narg('latitude'), sqlc.narg('longitude'),
    sqlc.narg('accuracy_meters'), sqlc.narg('distance_meters'), sqlc.narg('within_area'), sqlc.narg('qr_danru_id'),
    @late_minutes::int, @late_minutes_raw::int, sqlc.narg('shift_swap_id'), @now, @now)
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

-- ---------- Phase 07.6: DANRU absence, group present, admin correction ----------

-- The DANRU marking a team member absent (or a group-present entry marked ABSENT), and an admin
-- correction turning a record into ABSENT: one row per shift instance, like a check-in. No
-- ON CONFLICT DO NOTHING here (unlike InsertAttendanceCheckIn): the caller already read
-- GetAttendanceOfInstance and refused on a conflict before calling this, so a unique violation here
-- is a genuine race, left to surface as a 500 and be retried rather than silently returning nothing.
-- name: InsertAttendanceAbsent :one
INSERT INTO attendances (
    personnel_id, team_code, project_code, work_date, shift_id, shift_starts_at, shift_ends_at, timezone,
    status, marked_absent_by_id, absent_reason, shift_swap_id, created_at, updated_at)
VALUES (
    @personnel_id, @team_code, @project_code, @work_date, @shift_id, @shift_starts_at, @shift_ends_at, @timezone,
    'ABSENT', @marked_absent_by_id, sqlc.narg('absent_reason'), sqlc.narg('shift_swap_id'), @now, @now)
RETURNING *;

-- The regu monitor (GET /attendances, DANRU): per member and day, with the person's name and the
-- shift's, newest first. A deleted shift keeps its record but not its name (LEFT JOIN).
-- name: ListTeamAttendanceRows :many
-- Scope: a record of one of team_codes (the regu the caller's group/absence view reaches), or the
-- caller's own (self_id, set when they hold either permission, even as SELF only: a MEMBER sees
-- their own mangkir history and nothing of the regu). "swapped" is a LEAVE/ABSENT record that has an
-- active shift swap.
SELECT a.id, a.personnel_id, p.full_name, a.work_date, COALESCE(sh.name, '') AS shift_name,
  a.status, a.check_in_at, a.check_in_method, a.check_in_within_area,
  a.check_out_at, a.check_out_method, a.check_out_within_area,
  EXISTS (SELECT 1 FROM shift_swaps sw WHERE sw.replaced_attendance_id = a.id AND sw.cancelled_at IS NULL) AS swapped
FROM attendances a
JOIN personnels p ON p.id = a.personnel_id
LEFT JOIN shifts sh ON sh.id = a.shift_id
WHERE (a.team_code = ANY(@team_codes::text[]) OR a.personnel_id = sqlc.narg('self_id')::bigint)
  AND (sqlc.narg('team_code')::text IS NULL OR a.team_code = sqlc.narg('team_code')::text)
  AND (sqlc.narg('project_code')::text IS NULL OR a.project_code = sqlc.narg('project_code')::text)
  AND (sqlc.narg('status')::text IS NULL OR a.status::text = sqlc.narg('status')::text)
  AND (sqlc.narg('from_date')::date IS NULL OR a.work_date >= sqlc.narg('from_date')::date)
  AND (sqlc.narg('to_date')::date IS NULL OR a.work_date <= sqlc.narg('to_date')::date)
ORDER BY a.work_date DESC, a.id DESC
LIMIT @page_limit OFFSET @page_offset;

-- name: CountTeamAttendanceRows :one
SELECT count(*) FROM attendances a
WHERE (a.team_code = ANY(@team_codes::text[]) OR a.personnel_id = sqlc.narg('self_id')::bigint)
  AND (sqlc.narg('team_code')::text IS NULL OR a.team_code = sqlc.narg('team_code')::text)
  AND (sqlc.narg('project_code')::text IS NULL OR a.project_code = sqlc.narg('project_code')::text)
  AND (sqlc.narg('status')::text IS NULL OR a.status::text = sqlc.narg('status')::text)
  AND (sqlc.narg('from_date')::date IS NULL OR a.work_date >= sqlc.narg('from_date')::date)
  AND (sqlc.narg('to_date')::date IS NULL OR a.work_date <= sqlc.narg('to_date')::date);

-- The regu monitor (GET /attendances, DANRU) and the admin monitor (GET /admin/attendances): team
-- scope and region scope are mutually exclusive (one is nil depending on who is asking), both sides
-- of an optional date range, newest first.
-- name: ListAttendances :many
SELECT * FROM attendances a
WHERE (sqlc.narg('team_codes')::text[] IS NULL OR a.team_code = ANY(sqlc.narg('team_codes')::text[]))
  AND (sqlc.narg('team_code')::text IS NULL OR a.team_code = sqlc.narg('team_code')::text)
  AND (sqlc.narg('project_code')::text IS NULL OR a.project_code = sqlc.narg('project_code')::text)
  AND (sqlc.narg('region_code')::text IS NULL OR EXISTS (
        SELECT 1 FROM projects pr
        JOIN clients cl ON cl.organization_id = pr.organization_id AND cl.code = pr.client_code
        JOIN branches b ON b.organization_id = cl.organization_id AND b.code = cl.branch_code
        WHERE pr.code = a.project_code AND b.region_code = sqlc.narg('region_code')::text))
  AND (sqlc.narg('status')::text IS NULL OR a.status::text = sqlc.narg('status')::text)
  AND (sqlc.narg('sync_status')::text IS NULL OR a.personel_sync_status::text = sqlc.narg('sync_status')::text)
  AND (sqlc.narg('from_date')::date IS NULL OR a.work_date >= sqlc.narg('from_date')::date)
  AND (sqlc.narg('to_date')::date IS NULL OR a.work_date <= sqlc.narg('to_date')::date)
ORDER BY a.work_date DESC, a.id DESC
LIMIT @page_limit OFFSET @page_offset;

-- name: CountAttendances :one
SELECT count(*) FROM attendances a
WHERE (sqlc.narg('team_codes')::text[] IS NULL OR a.team_code = ANY(sqlc.narg('team_codes')::text[]))
  AND (sqlc.narg('team_code')::text IS NULL OR a.team_code = sqlc.narg('team_code')::text)
  AND (sqlc.narg('project_code')::text IS NULL OR a.project_code = sqlc.narg('project_code')::text)
  AND (sqlc.narg('region_code')::text IS NULL OR EXISTS (
        SELECT 1 FROM projects pr
        JOIN clients cl ON cl.organization_id = pr.organization_id AND cl.code = pr.client_code
        JOIN branches b ON b.organization_id = cl.organization_id AND b.code = cl.branch_code
        WHERE pr.code = a.project_code AND b.region_code = sqlc.narg('region_code')::text))
  AND (sqlc.narg('status')::text IS NULL OR a.status::text = sqlc.narg('status')::text)
  AND (sqlc.narg('sync_status')::text IS NULL OR a.personel_sync_status::text = sqlc.narg('sync_status')::text)
  AND (sqlc.narg('from_date')::date IS NULL OR a.work_date >= sqlc.narg('from_date')::date)
  AND (sqlc.narg('to_date')::date IS NULL OR a.work_date <= sqlc.narg('to_date')::date);

-- The regu's per-day counts for GET /attendances/group-summary, one GROUP BY query instead of a
-- loop per day: how many of the team's scheduled, non-OFF shift instances landed in each bucket.
-- "missing" has no record at all yet, whether the shift already ended or not (there is no
-- UPCOMING bucket here, unlike the personal calendar of 07.4). "swapped" (07.7) is a LEAVE/ABSENT
-- record with an active shift_swaps row: covered, not missing.
-- name: GroupSummary :many
SELECT s.work_date,
  count(*)::int AS scheduled,
  count(*) FILTER (WHERE a.status = 'PRESENT')::int AS present,
  count(*) FILTER (WHERE a.status = 'LEAVE')::int AS leave,
  count(*) FILTER (WHERE a.status = 'ABSENT')::int AS absent,
  count(*) FILTER (WHERE a.id IS NULL)::int AS missing,
  count(*) FILTER (WHERE EXISTS (
    SELECT 1 FROM shift_swaps sw WHERE sw.replaced_attendance_id = a.id AND sw.cancelled_at IS NULL))::int AS swapped
FROM schedules s
JOIN shifts sh ON sh.id = s.shift_id AND NOT sh.is_off
JOIN team_members tm ON tm.personnel_id = s.personnel_id AND tm.team_code = @team_code
  AND s.work_date >= tm.start_date AND (tm.end_date IS NULL OR s.work_date <= tm.end_date)
LEFT JOIN attendances a ON a.personnel_id = s.personnel_id AND a.team_code = @team_code
  AND a.work_date = s.work_date AND a.shift_id = s.shift_id
WHERE s.work_date BETWEEN @date_from AND @date_to
GROUP BY s.work_date
ORDER BY s.work_date;

-- The DANRU undoing their own mistaken mangkir (DELETE /attendances/absent/{id}): the row is gone
-- for real, there is no "no record" status to revert it to.
-- name: DeleteAttendance :exec
DELETE FROM attendances WHERE id = @id;

-- Updates whichever of check-in, check-out and absence an admin correction (PATCH
-- /admin/attendances/{id}) touches; always stamps the capture method FORCE on the side it edits,
-- and always marks personel_sync_status PENDING so the correction reaches Persist Personel. The
-- caller reads the row with GetAttendanceForUpdate first and passes back every field, changed or
-- not: sqlc.narg lets a field that a correction does not touch keep its current value only when
-- the caller re-sends it, so the Go side (not SQL) decides what "untouched" means per transition.
-- name: CorrectAttendance :one
UPDATE attendances SET
    status = @status, marked_absent_by_id = sqlc.narg('marked_absent_by_id'), absent_reason = sqlc.narg('absent_reason'),
    check_in_at = sqlc.narg('check_in_at'), check_in_method = sqlc.narg('check_in_method'), late_minutes = @late_minutes::int,
    check_out_at = sqlc.narg('check_out_at'), check_out_method = sqlc.narg('check_out_method'),
    -- A record that is no longer LEAVE is no longer the leave's: its revoke must not touch it.
    leave_request_id = CASE WHEN @status::attendance_status = 'LEAVE' THEN leave_request_id END,
    leave_replaced_absent = CASE WHEN @status::attendance_status = 'LEAVE' THEN leave_replaced_absent ELSE false END,
    leave_prev_absent_by_id = CASE WHEN @status::attendance_status = 'LEAVE' THEN leave_prev_absent_by_id END,
    leave_prev_absent_reason = CASE WHEN @status::attendance_status = 'LEAVE' THEN leave_prev_absent_reason END,
    personel_sync_status = 'PENDING', updated_at = @now
WHERE id = @id
RETURNING *;
