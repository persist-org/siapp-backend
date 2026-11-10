-- Shift swap (tukar jaga, module 07.7). Everything runs inside database.WithTenant.

-- LEAVE/ABSENT records of a regu and date that have no active swap: GET /shift-swaps/replaceable.
-- name: ListReplaceableAttendances :many
SELECT a.* FROM attendances a
WHERE a.team_code = @team_code AND a.work_date = @work_date AND a.status IN ('LEAVE', 'ABSENT')
  AND NOT EXISTS (
    SELECT 1 FROM shift_swaps sw WHERE sw.replaced_attendance_id = a.id AND sw.cancelled_at IS NULL)
ORDER BY a.id;

-- Members of a regu on a date who are not on leave or absent that day: GET /shift-swaps/candidates.
-- A person already on an attendance record of PRESENT (checked in elsewhere, including as another
-- swap's replacer) is still a candidate: the overlap check in Create is what actually refuses them.
-- name: ListSwapCandidates :many
SELECT p.id AS personnel_id, p.full_name
FROM team_members tm
JOIN personnels p ON p.id = tm.personnel_id
WHERE tm.team_code = @team_code
  AND @work_date BETWEEN tm.start_date AND COALESCE(tm.end_date, 'infinity'::date)
  AND NOT EXISTS (
    SELECT 1 FROM attendances a
    WHERE a.personnel_id = tm.personnel_id AND a.work_date = @work_date AND a.status IN ('LEAVE', 'ABSENT'))
ORDER BY p.full_name;

-- name: InsertShiftSwap :one
INSERT INTO shift_swaps (
    replaced_personnel_id, replaced_attendance_id, replacing_personnel_id,
    team_code, work_date, shift_id, starts_at, ends_at, note, created_by_id, created_at, updated_at)
VALUES (
    @replaced_personnel_id, @replaced_attendance_id, @replacing_personnel_id,
    @team_code, @work_date, @shift_id, @starts_at, @ends_at, sqlc.narg('note'), @created_by_id, @now, @now)
RETURNING *;

-- name: GetShiftSwap :one
SELECT * FROM shift_swaps WHERE id = @id;

-- name: GetShiftSwapForUpdate :one
SELECT * FROM shift_swaps WHERE id = @id FOR UPDATE;

-- name: CancelShiftSwap :one
UPDATE shift_swaps SET cancelled_at = @now, updated_at = @now WHERE id = @id RETURNING *;

-- Cancels the active swaps of one record whose replacer has not checked in yet. Used before the
-- record is deleted or corrected back to PRESENT: a swap of a guard who is (again) at work covers
-- nothing. A swap whose replacer already started stays active; HasActiveSwapOfAttendance tells the
-- caller so it can refuse instead.
-- name: CancelUnstartedSwapsOfAttendance :exec
UPDATE shift_swaps sw SET cancelled_at = @now, updated_at = @now
WHERE sw.replaced_attendance_id = @attendance_id AND sw.cancelled_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM attendances r WHERE r.shift_swap_id = sw.id AND r.check_in_at IS NOT NULL);

-- name: HasActiveSwapOfAttendance :one
SELECT EXISTS (
  SELECT 1 FROM shift_swaps WHERE replaced_attendance_id = @attendance_id AND cancelled_at IS NULL);

-- Same as CancelUnstartedSwapsOfAttendance for every LEAVE row of a leave request about to be
-- revoked (pkg/leave runs it just before deleting those rows).
-- name: CancelUnstartedSwapsOfLeaveRequest :exec
UPDATE shift_swaps sw SET cancelled_at = @now, updated_at = @now
WHERE sw.cancelled_at IS NULL
  AND sw.replaced_attendance_id IN (
    SELECT a.id FROM attendances a
    WHERE a.leave_request_id = @leave_request_id AND a.status = 'LEAVE' AND a.check_in_at IS NULL)
  AND NOT EXISTS (SELECT 1 FROM attendances r WHERE r.shift_swap_id = sw.id AND r.check_in_at IS NOT NULL);

-- The DANRU's list: active or cancelled swaps of the teams their attendance.swap.create reaches.
-- name: ListShiftSwaps :many
SELECT * FROM shift_swaps
WHERE team_code = ANY(@team_codes::text[])
  AND (sqlc.narg('active')::boolean IS NULL OR (cancelled_at IS NULL) = sqlc.narg('active')::boolean)
  AND (sqlc.narg('from_date')::date IS NULL OR work_date >= sqlc.narg('from_date')::date)
  AND (sqlc.narg('to_date')::date IS NULL OR work_date <= sqlc.narg('to_date')::date)
ORDER BY work_date DESC, id DESC;

-- Active swaps where personnelID is the replacer, in range: the extra shift instances Checkable
-- adds. A cancelled swap (or one superseded, impossible today since one record has at most one
-- active swap) is never offered.
-- name: ListActiveSwapsOfReplacer :many
SELECT * FROM shift_swaps
WHERE replacing_personnel_id = @personnel_id AND cancelled_at IS NULL
  AND work_date BETWEEN @date_from AND @date_to
ORDER BY work_date;

-- "Digantikan" (GET /attendances/replaced): the caller's own shifts someone else took over, with
-- the replacer's name and their check-in/out of the replaced shift (the replacer's OWN attendance
-- row, found by its shift_swap_id, not the replaced row).
-- name: ListReplacedSwaps :many
SELECT sw.id, sw.work_date, sh.name AS shift_name, p.full_name AS replacing_name,
       ra.check_in_at AS replacing_check_in_at, ra.check_out_at AS replacing_check_out_at
FROM shift_swaps sw
JOIN personnels p ON p.id = sw.replacing_personnel_id
LEFT JOIN shifts sh ON sh.id = sw.shift_id
LEFT JOIN attendances ra ON ra.shift_swap_id = sw.id AND ra.personnel_id = sw.replacing_personnel_id
WHERE sw.replaced_personnel_id = @personnel_id AND sw.cancelled_at IS NULL
  AND (sqlc.narg('from_date')::date IS NULL OR sw.work_date >= sqlc.narg('from_date')::date)
  AND (sqlc.narg('to_date')::date IS NULL OR sw.work_date <= sqlc.narg('to_date')::date)
ORDER BY sw.work_date DESC, sw.id DESC;

-- "Menggantikan" (GET /attendances/replacing): the mirror, the caller as the replacer.
-- name: ListReplacingSwaps :many
SELECT sw.id, sw.work_date, sh.name AS shift_name, p.full_name AS replaced_name,
       ra.check_in_at AS check_in_at, ra.check_out_at AS check_out_at
FROM shift_swaps sw
JOIN personnels p ON p.id = sw.replaced_personnel_id
LEFT JOIN shifts sh ON sh.id = sw.shift_id
LEFT JOIN attendances ra ON ra.shift_swap_id = sw.id AND ra.personnel_id = sw.replacing_personnel_id
WHERE sw.replacing_personnel_id = @personnel_id AND sw.cancelled_at IS NULL
  AND (sqlc.narg('from_date')::date IS NULL OR sw.work_date >= sqlc.narg('from_date')::date)
  AND (sqlc.narg('to_date')::date IS NULL OR sw.work_date <= sqlc.narg('to_date')::date)
ORDER BY sw.work_date DESC, sw.id DESC;
