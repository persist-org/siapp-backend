-- Phase 07.5 Part B: leave, permit and sick requests.

-- One statement, one row lock: two submits at once get different numbers.
-- name: NextLeaveRequestCounter :one
INSERT INTO leave_request_counters (type, counter) VALUES (@type, 1)
ON CONFLICT (organization_id, type) DO UPDATE SET counter = leave_request_counters.counter + 1
RETURNING counter;

-- name: InsertLeaveRequest :one
INSERT INTO leave_requests (
    number, personnel_id, team_code, project_code, type, category_id, start_date, end_date, start_time, end_time,
    working_days, reason, responsibility, contact_info, change_day_from, change_day_to,
    hospitalization, letter_status, letter_date, disease, approver_personnel_id, created_at, updated_at)
VALUES (
    @number, @personnel_id, @team_code, @project_code, @type, @category_id, @start_date, @end_date,
    sqlc.narg('start_time'), sqlc.narg('end_time'),
    @working_days, @reason, sqlc.narg('responsibility'), sqlc.narg('contact_info'),
    sqlc.narg('change_day_from'), sqlc.narg('change_day_to'),
    sqlc.narg('hospitalization'), sqlc.narg('letter_status'), sqlc.narg('letter_date'), sqlc.narg('disease'),
    sqlc.narg('approver_personnel_id'), @now, @now)
RETURNING *;

-- name: SetLeaveRequestAttachment :exec
UPDATE leave_requests SET attachment_media_id = @media_id WHERE id = @id;

-- name: GetLeaveRequestForUpdate :one
SELECT * FROM leave_requests WHERE id = @id FOR UPDATE;

-- A request with what the views show beside it: its category and the person who decided it.
-- name: GetLeaveRequestView :one
SELECT sqlc.embed(lr), c.code AS category_code, c.name AS category_name,
       COALESCE(d.full_name, '')::text AS decided_by_name
FROM leave_requests lr
JOIN leave_categories c ON c.id = lr.category_id
LEFT JOIN personnels d ON d.id = lr.decided_by_id
WHERE lr.id = @id;

-- name: ListLeaveRequestViewsOfPersonnel :many
SELECT sqlc.embed(lr), c.code AS category_code, c.name AS category_name,
       COALESCE(d.full_name, '')::text AS decided_by_name
FROM leave_requests lr
JOIN leave_categories c ON c.id = lr.category_id
LEFT JOIN personnels d ON d.id = lr.decided_by_id
WHERE lr.personnel_id = @personnel_id
  AND (cardinality(@types::text[]) = 0 OR lr.type::text = ANY(@types::text[]))
  AND (cardinality(@statuses::text[]) = 0 OR lr.status::text = ANY(@statuses::text[]))
  AND (sqlc.narg('from_date')::date IS NULL OR lr.end_date >= sqlc.narg('from_date')::date)
  AND (sqlc.narg('to_date')::date IS NULL OR lr.start_date <= sqlc.narg('to_date')::date)
ORDER BY lr.id DESC
LIMIT @page_limit OFFSET @page_offset;

-- name: CountLeaveRequestsOfPersonnel :one
SELECT count(*) FROM leave_requests lr
WHERE lr.personnel_id = @personnel_id
  AND (cardinality(@types::text[]) = 0 OR lr.type::text = ANY(@types::text[]))
  AND (cardinality(@statuses::text[]) = 0 OR lr.status::text = ANY(@statuses::text[]))
  AND (sqlc.narg('from_date')::date IS NULL OR lr.end_date >= sqlc.narg('from_date')::date)
  AND (sqlc.narg('to_date')::date IS NULL OR lr.start_date <= sqlc.narg('to_date')::date);

-- The person's live requests (pending or approved) that share a day with [start, end], and whether
-- each is a "part of day" one. A canceled or rejected request is not a candidate.
-- name: ListLiveLeaveRequestsOverlapping :many
SELECT lr.id, lr.start_date, lr.end_date, lr.start_time, lr.end_time, c.requires_time_range
FROM leave_requests lr
JOIN leave_categories c ON c.id = lr.category_id
WHERE lr.personnel_id = @personnel_id AND lr.status IN ('PENDING', 'APPROVED')
  AND lr.start_date <= @end_date AND lr.end_date >= @start_date
ORDER BY lr.id;

-- The working days pending and approved requests of a category hold, among those that start in
-- the year (the bounds) and end on or after not_before (annual leave: the days not yet taken).
-- name: SumLiveLeaveDays :one
SELECT COALESCE(sum(working_days), 0)::int AS days FROM leave_requests
WHERE personnel_id = @personnel_id AND category_id = @category_id AND status IN ('PENDING', 'APPROVED')
  AND start_date BETWEEN @year_start AND @year_end AND end_date >= @not_before;

-- name: CancelLeaveRequest :one
UPDATE leave_requests SET status = 'CANCELLED', cancelled_at = @now, updated_at = @now
WHERE id = @id AND status = 'PENDING'
RETURNING *;
