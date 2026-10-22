-- Shift assignments and schedules (PJ48 m_employee_work.shift_groub*, m_schedules).
-- Everything runs inside database.WithTenant. work_date is a business date in the project's
-- timezone.

-- name: GetShiftAssignment :one
SELECT * FROM personnel_shift_assignments WHERE personnel_id = $1;

-- Locks the person's assignment, so two changes of it (or a change and the nightly
-- extension) take turns.
-- name: GetShiftAssignmentForUpdate :one
SELECT * FROM personnel_shift_assignments WHERE personnel_id = $1 FOR UPDATE;

-- name: UpsertShiftAssignment :one
INSERT INTO personnel_shift_assignments (personnel_id, schedule_type, shift_group_id, effective_date,
                                         generated_until, updated_by, created_at, updated_at)
VALUES (@personnel_id, @schedule_type, @shift_group_id, @effective_date, @generated_until, @actor, @now, @now)
ON CONFLICT (organization_id, personnel_id) DO UPDATE SET
    schedule_type   = EXCLUDED.schedule_type,
    shift_group_id  = EXCLUDED.shift_group_id,
    effective_date  = EXCLUDED.effective_date,
    generated_until = EXCLUDED.generated_until,
    updated_by      = EXCLUDED.updated_by,
    updated_at      = EXCLUDED.updated_at
RETURNING *;

-- name: SetGeneratedUntil :exec
UPDATE personnel_shift_assignments SET generated_until = @generated_until WHERE personnel_id = @personnel_id;

-- The people whose FIXED window ends before the target date. Not locked: the extension then
-- takes each person in a short transaction of its own (ExtendOne), so it never holds the whole
-- organization's assignments at once. A person Personalia reports as inactive has left: their
-- window is not extended.
-- name: ListAssignmentsToExtend :many
SELECT a.personnel_id FROM personnel_shift_assignments a
JOIN personnels p ON p.id = a.personnel_id
WHERE a.schedule_type = 'FIXED' AND p.status <> 'INACTIVE'
  AND (a.generated_until IS NULL OR a.generated_until < @target)
ORDER BY a.personnel_id;

-- ⚠ The per-person lock of everything that rewrites a person's schedule (an assignment, the
-- extension of it). FOR NO KEY UPDATE, not FOR UPDATE: schedule inserts take the person's
-- KEY SHARE lock through their foreign key, which FOR UPDATE would conflict with.
-- name: LockPersonnelForSchedule :one
SELECT id, status FROM personnels WHERE id = $1 FOR NO KEY UPDATE;

-- Bounded above by the generator's horizon: a date after it was never generated, so it can only
-- be a manual entry, possibly of the regu the person moves to later, and it is left alone.
-- name: DeleteSchedulesOfPersonnelBetween :execrows
DELETE FROM schedules WHERE personnel_id = @personnel_id AND work_date BETWEEN @from_date AND @to_date;

-- Generates one row per date of [from, to] from the group's weekly pattern (ISO weekday:
-- Monday 1 .. Sunday 7). A date that already has a schedule keeps it: an admin's own entry is
-- never overwritten by the generator.
-- name: GenerateSchedules :execrows
INSERT INTO schedules (personnel_id, work_date, shift_id, shift_group_id, created_by, updated_by, created_at, updated_at)
SELECT @personnel_id, d::date, gd.shift_id, gd.shift_group_id, sqlc.narg('actor'), sqlc.narg('actor'), @now, @now
FROM generate_series(@from_date::date, @to_date::date, interval '1 day') AS d
JOIN shift_group_days gd ON gd.shift_group_id = @shift_group_id AND gd.weekday = extract(isodow FROM d)::smallint
ON CONFLICT (organization_id, personnel_id, work_date) DO NOTHING;

-- The schedules of a date range, with the person and the shift. scope_region is the
-- REGION_ADMIN's region: a row is theirs when the person serves in a regu of that region on
-- the row's date. team_code narrows to the people of one regu on the row's date.
-- name: ListSchedules :many
SELECT s.id, s.personnel_id, s.work_date, s.shift_group_id, s.created_by, s.updated_by, s.created_at, s.updated_at,
       p.full_name, p.nik,
       sh.id AS shift_id, sh.name AS shift_name, sh.start_minute, sh.end_minute, sh.is_off
FROM schedules s
JOIN personnels p ON p.id = s.personnel_id
JOIN shifts sh ON sh.id = s.shift_id
WHERE s.work_date BETWEEN @date_from AND @date_to
  AND (sqlc.narg('personnel_id')::bigint IS NULL OR s.personnel_id = sqlc.narg('personnel_id'))
  AND (sqlc.narg('q')::text IS NULL OR strpos(lower(p.full_name), lower(sqlc.narg('q'))) > 0 OR p.nik = sqlc.narg('q'))
  AND (sqlc.narg('team_code')::text IS NULL OR EXISTS (
        SELECT 1 FROM team_members tm
        WHERE tm.personnel_id = s.personnel_id AND tm.team_code = sqlc.narg('team_code')
          AND tm.start_date <= s.work_date AND (tm.end_date IS NULL OR tm.end_date >= s.work_date)))
  AND (sqlc.narg('scope_region')::text IS NULL OR EXISTS (
        SELECT 1 FROM team_members tm
        WHERE tm.personnel_id = s.personnel_id
          AND 'OS' || substring(tm.team_code from '^[0-9]+') = sqlc.narg('scope_region')
          AND tm.start_date <= s.work_date AND (tm.end_date IS NULL OR tm.end_date >= s.work_date)))
ORDER BY s.work_date, lower(p.full_name), s.id
LIMIT @page_limit OFFSET @page_offset;

-- name: CountSchedules :one
SELECT count(*)
FROM schedules s
JOIN personnels p ON p.id = s.personnel_id
WHERE s.work_date BETWEEN @date_from AND @date_to
  AND (sqlc.narg('personnel_id')::bigint IS NULL OR s.personnel_id = sqlc.narg('personnel_id'))
  AND (sqlc.narg('q')::text IS NULL OR strpos(lower(p.full_name), lower(sqlc.narg('q'))) > 0 OR p.nik = sqlc.narg('q'))
  AND (sqlc.narg('team_code')::text IS NULL OR EXISTS (
        SELECT 1 FROM team_members tm
        WHERE tm.personnel_id = s.personnel_id AND tm.team_code = sqlc.narg('team_code')
          AND tm.start_date <= s.work_date AND (tm.end_date IS NULL OR tm.end_date >= s.work_date)))
  AND (sqlc.narg('scope_region')::text IS NULL OR EXISTS (
        SELECT 1 FROM team_members tm
        WHERE tm.personnel_id = s.personnel_id
          AND 'OS' || substring(tm.team_code from '^[0-9]+') = sqlc.narg('scope_region')
          AND tm.start_date <= s.work_date AND (tm.end_date IS NULL OR tm.end_date >= s.work_date)));

-- name: GetScheduleView :one
SELECT s.id, s.personnel_id, s.work_date, s.shift_group_id, s.created_by, s.updated_by, s.created_at, s.updated_at,
       p.full_name, p.nik,
       sh.id AS shift_id, sh.name AS shift_name, sh.start_minute, sh.end_minute, sh.is_off
FROM schedules s
JOIN personnels p ON p.id = s.personnel_id
JOIN shifts sh ON sh.id = s.shift_id
WHERE s.id = $1;

-- name: GetScheduleForUpdate :one
SELECT * FROM schedules WHERE id = $1 FOR UPDATE;

-- name: InsertSchedule :one
INSERT INTO schedules (personnel_id, work_date, shift_id, created_by, updated_by, created_at, updated_at)
VALUES (@personnel_id, @work_date, @shift_id, @actor, @actor, @now, @now)
RETURNING id;

-- A schedule changed by hand is no longer the group's: shift_group_id becomes NULL.
-- name: UpdateSchedule :exec
UPDATE schedules SET work_date = @work_date, shift_id = @shift_id, shift_group_id = NULL,
                     updated_by = @actor, updated_at = @now
WHERE id = @id;

-- name: DeleteSchedule :execrows
DELETE FROM schedules WHERE id = $1;

-- The schedules of a person over a date range, with the shift, for the shift instances.
-- name: ListSchedulesOfPersonnelBetween :many
SELECT s.id, s.work_date, sh.id AS shift_id, sh.name AS shift_name, sh.start_minute, sh.end_minute,
       sh.is_off, sh.tolerance_minutes
FROM schedules s
JOIN shifts sh ON sh.id = s.shift_id
WHERE s.personnel_id = @personnel_id AND s.work_date BETWEEN @date_from AND @date_to
ORDER BY s.work_date;

-- The person's regu over a date range with each one's project and timezone: what decides
-- where (and in which timezone) a schedule on a date is worked.
-- name: ListMembershipZonesOfPersonnelBetween :many
SELECT tm.start_date, tm.end_date, t.project_code, pr.timezone
FROM team_members tm
JOIN teams t     ON t.organization_id = tm.organization_id AND t.code = tm.team_code
JOIN projects pr ON pr.organization_id = t.organization_id AND pr.code = t.project_code
WHERE tm.personnel_id = @personnel_id AND t.deleted_at IS NULL
  AND tm.start_date <= @date_to AND (tm.end_date IS NULL OR tm.end_date >= @date_from)
ORDER BY tm.start_date, t.project_code;

-- name: PersonnelInRegionOn :one
SELECT EXISTS (
    SELECT 1 FROM team_members tm
    WHERE tm.personnel_id = @personnel_id
      AND 'OS' || substring(tm.team_code from '^[0-9]+') = @region_code::text
      AND tm.start_date <= @on_date AND (tm.end_date IS NULL OR tm.end_date >= @on_date));

-- The first date of [from, to] on which the person serves in no regu of the region, if any: a
-- REGION_ADMIN may regenerate a range only when every date of it is theirs.
-- name: FirstDateOutsideRegion :one
SELECT d::date AS outside_date
FROM generate_series(@from_date::date, @to_date::date, interval '1 day') AS d
WHERE NOT EXISTS (
    SELECT 1 FROM team_members tm
    WHERE tm.personnel_id = @personnel_id
      AND 'OS' || substring(tm.team_code from '^[0-9]+') = @region_code::text
      AND tm.start_date <= d::date AND (tm.end_date IS NULL OR tm.end_date >= d::date))
ORDER BY d
LIMIT 1;

-- A person is within a REGION_ADMIN's reach for their assignment when they serve, or will
-- serve, in a regu of the region: a membership in it that has not ended before its project's
-- today.
-- name: PersonnelInRegionFromNow :one
SELECT EXISTS (
    SELECT 1 FROM team_members tm
    JOIN teams t     ON t.organization_id = tm.organization_id AND t.code = tm.team_code
    JOIN projects pr ON pr.organization_id = t.organization_id AND pr.code = t.project_code
    WHERE tm.personnel_id = @personnel_id AND t.deleted_at IS NULL
      AND 'OS' || substring(tm.team_code from '^[0-9]+') = @region_code::text
      AND (tm.end_date IS NULL OR tm.end_date >= (sqlc.arg('now')::timestamptz AT TIME ZONE pr.timezone)::date));
