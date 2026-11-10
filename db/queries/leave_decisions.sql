-- Phase 07.5 Part C: deciding a request, and what an approval does to attendance.

-- name: DecideLeaveRequest :one
UPDATE leave_requests
SET status = @status, decided_level = @decided_level, decided_by_id = @decided_by_id, decided_at = @now,
    decision_note = sqlc.narg('note'), updated_at = @now
WHERE id = @id
RETURNING *;

-- name: GetLeaveRequest :one
SELECT * FROM leave_requests WHERE id = @id;

-- name: GetLeaveCategoryByID :one
SELECT * FROM leave_categories WHERE id = @id;

-- name: GetLeavePersonName :one
SELECT full_name FROM personnels WHERE id = @id;

-- The region a project sits under: what a REGION_ADMIN's reach over a request is checked against.
-- name: GetRegionCodeOfProject :one
SELECT b.region_code
FROM projects pr
JOIN clients cl ON cl.organization_id = pr.organization_id AND cl.code = pr.client_code
JOIN branches b ON b.organization_id = cl.organization_id AND b.code = cl.branch_code
WHERE pr.code = @code;

-- One row per request the approver, or an admin, may see. team_codes NULL means every regu (an
-- admin); team_code, project_code and region_code narrow further. The list is by regu, NOT by the
-- frozen approver_personnel_id: the DANRU of a regu changes, a request does not.
-- name: ListLeaveRequestViews :many
SELECT sqlc.embed(lr), c.code AS category_code, c.name AS category_name,
       COALESCE(d.full_name, '')::text AS decided_by_name,
       p.full_name AS person_name, COALESCE(p.position_name, '')::text AS person_position
FROM leave_requests lr
JOIN leave_categories c ON c.id = lr.category_id
JOIN personnels p ON p.id = lr.personnel_id
LEFT JOIN personnels d ON d.id = lr.decided_by_id
WHERE (sqlc.narg('team_codes')::text[] IS NULL OR lr.team_code = ANY(sqlc.narg('team_codes')::text[]))
  AND (sqlc.narg('team_code')::text IS NULL OR lr.team_code = sqlc.narg('team_code')::text)
  AND (sqlc.narg('project_code')::text IS NULL OR lr.project_code = sqlc.narg('project_code')::text)
  AND (sqlc.narg('region_code')::text IS NULL OR EXISTS (
        SELECT 1 FROM projects pr
        JOIN clients cl ON cl.organization_id = pr.organization_id AND cl.code = pr.client_code
        JOIN branches b ON b.organization_id = cl.organization_id AND b.code = cl.branch_code
        WHERE pr.code = lr.project_code AND b.region_code = sqlc.narg('region_code')::text))
  AND (cardinality(@types::text[]) = 0 OR lr.type::text = ANY(@types::text[]))
  AND (cardinality(@statuses::text[]) = 0 OR lr.status::text = ANY(@statuses::text[]))
  AND (sqlc.narg('from_date')::date IS NULL OR lr.end_date >= sqlc.narg('from_date')::date)
  AND (sqlc.narg('to_date')::date IS NULL OR lr.start_date <= sqlc.narg('to_date')::date)
  AND (@q::text = '' OR p.full_name ILIKE '%' || @q::text || '%' OR lr.number ILIKE '%' || @q::text || '%')
ORDER BY lr.id DESC
LIMIT @page_limit OFFSET @page_offset;

-- name: CountLeaveRequestViews :one
SELECT count(*)
FROM leave_requests lr
JOIN personnels p ON p.id = lr.personnel_id
WHERE (sqlc.narg('team_codes')::text[] IS NULL OR lr.team_code = ANY(sqlc.narg('team_codes')::text[]))
  AND (sqlc.narg('team_code')::text IS NULL OR lr.team_code = sqlc.narg('team_code')::text)
  AND (sqlc.narg('project_code')::text IS NULL OR lr.project_code = sqlc.narg('project_code')::text)
  AND (sqlc.narg('region_code')::text IS NULL OR EXISTS (
        SELECT 1 FROM projects pr
        JOIN clients cl ON cl.organization_id = pr.organization_id AND cl.code = pr.client_code
        JOIN branches b ON b.organization_id = cl.organization_id AND b.code = cl.branch_code
        WHERE pr.code = lr.project_code AND b.region_code = sqlc.narg('region_code')::text))
  AND (cardinality(@types::text[]) = 0 OR lr.type::text = ANY(@types::text[]))
  AND (cardinality(@statuses::text[]) = 0 OR lr.status::text = ANY(@statuses::text[]))
  AND (sqlc.narg('from_date')::date IS NULL OR lr.end_date >= sqlc.narg('from_date')::date)
  AND (sqlc.narg('to_date')::date IS NULL OR lr.start_date <= sqlc.narg('to_date')::date)
  AND (@q::text = '' OR p.full_name ILIKE '%' || @q::text || '%' OR lr.number ILIKE '%' || @q::text || '%');

-- name: GetLeaveRequestViewWithPerson :one
SELECT sqlc.embed(lr), c.code AS category_code, c.name AS category_name,
       COALESCE(d.full_name, '')::text AS decided_by_name,
       p.full_name AS person_name, COALESCE(p.position_name, '')::text AS person_position
FROM leave_requests lr
JOIN leave_categories c ON c.id = lr.category_id
JOIN personnels p ON p.id = lr.personnel_id
LEFT JOIN personnels d ON d.id = lr.decided_by_id
WHERE lr.id = @id;

-- Other members of the regu with an approved request that shares a day with [start, end]: what the
-- approver weighs before saying yes.
-- name: ListApprovedLeaveOverlapping :many
SELECT lr.start_date, lr.end_date, p.full_name
FROM leave_requests lr
JOIN personnels p ON p.id = lr.personnel_id
WHERE lr.team_code = @team_code AND lr.status = 'APPROVED' AND lr.personnel_id <> @personnel_id
  AND lr.start_date <= @end_date AND lr.end_date >= @start_date
ORDER BY lr.start_date, lr.id;

-- name: CountTeamMembersOnLeave :one
SELECT count(DISTINCT personnel_id) FROM leave_requests
WHERE team_code = @team_code AND status = 'APPROVED' AND personnel_id <> @personnel_id
  AND start_date <= @on_date AND end_date >= @on_date;

-- A LEAVE record for one shift instance. A PRESENT record stays: the guard already came to work,
-- so the conflict updates nothing and no row comes back (the caller lists it as skipped). An
-- ABSENT record becomes LEAVE, as KPI's alpa is false under a permit.
-- name: UpsertLeaveAttendance :one
INSERT INTO attendances (
    personnel_id, team_code, project_code, work_date, shift_id, shift_starts_at, shift_ends_at, timezone,
    status, leave_request_id, created_at, updated_at)
VALUES (
    @personnel_id, @team_code, @project_code, @work_date, @shift_id, @shift_starts_at, @shift_ends_at, @timezone,
    'LEAVE', @leave_request_id, @now, @now)
ON CONFLICT (organization_id, personnel_id, team_code, work_date, shift_id) DO UPDATE
SET status = 'LEAVE', leave_request_id = EXCLUDED.leave_request_id, marked_absent_by_id = NULL, absent_reason = NULL,
    -- The mangkir it replaces is remembered (who marked it and why), so a revoke can put it back.
    leave_replaced_absent = attendances.leave_replaced_absent OR attendances.status = 'ABSENT',
    leave_prev_absent_by_id = CASE WHEN attendances.status = 'ABSENT' THEN attendances.marked_absent_by_id ELSE attendances.leave_prev_absent_by_id END,
    leave_prev_absent_reason = CASE WHEN attendances.status = 'ABSENT' THEN attendances.absent_reason ELSE attendances.leave_prev_absent_reason END,
    updated_at = EXCLUDED.updated_at
WHERE attendances.status <> 'PRESENT'
RETURNING id;

-- A revoked leave gives back the mangkir its approval had turned into LEAVE.
-- name: RestoreAbsentOfLeaveRequest :execrows
UPDATE attendances SET
    status = 'ABSENT', marked_absent_by_id = leave_prev_absent_by_id, absent_reason = leave_prev_absent_reason,
    leave_request_id = NULL, leave_replaced_absent = false, leave_prev_absent_by_id = NULL, leave_prev_absent_reason = NULL,
    updated_at = @now
WHERE leave_request_id = @leave_request_id AND status = 'LEAVE' AND check_in_at IS NULL AND leave_replaced_absent;

-- The LEAVE rows of a request that nobody has checked into. A row that somehow has a check-in keeps
-- its evidence (DetachLeaveAttendancesOfRequest only drops the link). Run after
-- RestoreAbsentOfLeaveRequest, which takes the rows that were a mangkir first.
-- name: DeleteLeaveAttendancesOfRequest :execrows
DELETE FROM attendances WHERE leave_request_id = @leave_request_id AND status = 'LEAVE' AND check_in_at IS NULL;

-- name: DetachLeaveAttendancesOfRequest :exec
UPDATE attendances SET leave_request_id = NULL WHERE leave_request_id = @leave_request_id;

-- name: ListApprovedLeaveCovering :many
SELECT lr.id, lr.start_time, lr.end_time, c.requires_time_range, c.excuses_lateness
FROM leave_requests lr
JOIN leave_categories c ON c.id = lr.category_id
WHERE lr.personnel_id = @personnel_id AND lr.status = 'APPROVED'
  AND lr.start_date <= @on_date AND lr.end_date >= @on_date;

-- Whether a request of any category that is still alive (pending or approved) covers a day: such a
-- day is explained, whatever its attendance looks like.
-- name: HasLiveLeaveCovering :one
SELECT EXISTS (
    SELECT 1 FROM leave_requests
    WHERE personnel_id = @personnel_id AND status IN ('PENDING', 'APPROVED')
      AND start_date <= @on_date AND end_date >= @on_date);

-- name: ListCheckedInAttendancesBetween :many
SELECT * FROM attendances
WHERE personnel_id = @personnel_id AND work_date BETWEEN @date_from AND @date_to
  AND status = 'PRESENT' AND check_in_at IS NOT NULL
ORDER BY work_date, id
FOR UPDATE;

-- name: SetAttendanceLateMinutes :exec
UPDATE attendances SET late_minutes = @late_minutes, updated_at = @now WHERE id = @id;
