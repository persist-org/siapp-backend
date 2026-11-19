-- Patrol schedules (Phase 10.11): a person is scheduled to patrol a zone in a time window.

-- name: InsertPatrolSchedule :one
INSERT INTO patrol_schedules (project_code, zone_id, personnel_id, starts_at, ends_at, work_date, notes, created_by_id, created_at, updated_at)
VALUES (@project_code, @zone_id, @personnel_id, @starts_at, @ends_at, @work_date, sqlc.narg('notes'), @created_by_id, @now, @now)
RETURNING *;

-- name: GetPatrolSchedule :one
SELECT * FROM patrol_schedules WHERE id = @id;

-- name: GetPatrolScheduleForUpdate :one
SELECT * FROM patrol_schedules WHERE id = @id FOR UPDATE;

-- Every column an admin can change, written together: the usecase has merged the patch into the row it locked.
-- name: UpdatePatrolSchedule :one
UPDATE patrol_schedules
SET personnel_id = @personnel_id, starts_at = @starts_at, ends_at = @ends_at, work_date = @work_date,
    notes = sqlc.narg('notes'), updated_at = @now
WHERE id = @id
RETURNING *;

-- name: CancelPatrolSchedule :one
UPDATE patrol_schedules SET cancelled_at = @now, cancelled_by_id = @cancelled_by_id, updated_at = @now
WHERE id = @id AND cancelled_at IS NULL
RETURNING *;

-- The live schedules of a person that overlap [from, to): two windows overlap when each starts before the
-- other ends. except_id leaves the schedule being edited out.
-- name: ListOverlappingSchedules :many
SELECT * FROM patrol_schedules
WHERE personnel_id = @personnel_id AND cancelled_at IS NULL
  AND starts_at < @window_end AND ends_at > @window_start
  AND (sqlc.narg('except_id')::uuid IS NULL OR id <> sqlc.narg('except_id'))
ORDER BY starts_at, id;

-- The schedules a reader may see, with the zone and the person. One statement for the admin and the field:
--   * the admin passes scope_region (their region) and no viewer;
--   * the field passes the viewer, who sees their own, and the people of the regu they decide for (patrol_teams
--     for a PATROL zone, guard_teams for a GUARD one: the teams of their approve permission) who belong to a
--     regu of the schedule's project on the work date.
-- The status is a function of the clock and of the patrol (see scheduleStatus), so the filter spells it out: CANCELLED, MISSED
-- (no patrol, over), SCHEDULED (no patrol, not over), and from 10.12 IN_PROGRESS and COMPLETED by the patrol started from it.
-- name: ListPatrolSchedules :many
SELECT sc.*, z.name AS zone_name, z.type AS zone_type, p.full_name AS personnel_name,
       pr.id AS patrol_id, pr.status AS patrol_status, pr.started_at AS patrol_started_at,
       (SELECT count(*) FROM patrol_scans s WHERE s.patrol_id = pr.id AND s.zone_id = sc.zone_id AND s.outcome = 'SCANNED')::int AS scan_count
FROM patrol_schedules sc
JOIN patrol_zones z ON z.organization_id = sc.organization_id AND z.id = sc.zone_id
JOIN personnels p ON p.id = sc.personnel_id
LEFT JOIN patrol_routes pr ON pr.organization_id = sc.organization_id AND pr.schedule_id = sc.id
WHERE (sqlc.narg('scope_region')::text IS NULL OR 'OS' || substring(sc.project_code from '^[0-9]+') = sqlc.narg('scope_region'))
  AND (sqlc.narg('viewer_id')::bigint IS NULL OR sc.personnel_id = sqlc.narg('viewer_id') OR EXISTS (
        SELECT 1 FROM team_members tm
        JOIN teams t ON t.organization_id = tm.organization_id AND t.code = tm.team_code
        WHERE tm.personnel_id = sc.personnel_id AND t.project_code = sc.project_code AND t.deleted_at IS NULL
          AND tm.start_date <= sc.work_date AND (tm.end_date IS NULL OR tm.end_date >= sc.work_date)
          AND tm.team_code = ANY(CASE WHEN z.type = 'PATROL' THEN @patrol_teams::text[] ELSE @guard_teams::text[] END)))
  AND (sqlc.narg('project_code')::text IS NULL OR sc.project_code = sqlc.narg('project_code'))
  AND (sqlc.narg('zone_id')::bigint IS NULL OR sc.zone_id = sqlc.narg('zone_id'))
  AND (sqlc.narg('personnel_id')::bigint IS NULL OR sc.personnel_id = sqlc.narg('personnel_id'))
  AND (sqlc.narg('date_from')::date IS NULL OR sc.work_date >= sqlc.narg('date_from'))
  AND (sqlc.narg('date_to')::date IS NULL OR sc.work_date <= sqlc.narg('date_to'))
  AND (sqlc.narg('status')::text IS NULL
       OR (sqlc.narg('status') = 'CANCELLED' AND sc.cancelled_at IS NOT NULL)
       OR (sqlc.narg('status') = 'IN_PROGRESS' AND sc.cancelled_at IS NULL AND pr.status = 'ACTIVE')
       OR (sqlc.narg('status') = 'COMPLETED' AND sc.cancelled_at IS NULL AND pr.status IN ('ENDED', 'AUTO_ENDED'))
       OR (sqlc.narg('status') = 'MISSED' AND sc.cancelled_at IS NULL AND pr.id IS NULL AND sc.ends_at < @now)
       OR (sqlc.narg('status') = 'SCHEDULED' AND sc.cancelled_at IS NULL AND pr.id IS NULL AND sc.ends_at >= @now))
ORDER BY sc.starts_at, sc.id
LIMIT @page_limit OFFSET @page_offset;

-- name: CountPatrolSchedules :one
SELECT count(*)
FROM patrol_schedules sc
JOIN patrol_zones z ON z.organization_id = sc.organization_id AND z.id = sc.zone_id
LEFT JOIN patrol_routes pr ON pr.organization_id = sc.organization_id AND pr.schedule_id = sc.id
WHERE (sqlc.narg('scope_region')::text IS NULL OR 'OS' || substring(sc.project_code from '^[0-9]+') = sqlc.narg('scope_region'))
  AND (sqlc.narg('viewer_id')::bigint IS NULL OR sc.personnel_id = sqlc.narg('viewer_id') OR EXISTS (
        SELECT 1 FROM team_members tm
        JOIN teams t ON t.organization_id = tm.organization_id AND t.code = tm.team_code
        WHERE tm.personnel_id = sc.personnel_id AND t.project_code = sc.project_code AND t.deleted_at IS NULL
          AND tm.start_date <= sc.work_date AND (tm.end_date IS NULL OR tm.end_date >= sc.work_date)
          AND tm.team_code = ANY(CASE WHEN z.type = 'PATROL' THEN @patrol_teams::text[] ELSE @guard_teams::text[] END)))
  AND (sqlc.narg('project_code')::text IS NULL OR sc.project_code = sqlc.narg('project_code'))
  AND (sqlc.narg('zone_id')::bigint IS NULL OR sc.zone_id = sqlc.narg('zone_id'))
  AND (sqlc.narg('personnel_id')::bigint IS NULL OR sc.personnel_id = sqlc.narg('personnel_id'))
  AND (sqlc.narg('date_from')::date IS NULL OR sc.work_date >= sqlc.narg('date_from'))
  AND (sqlc.narg('date_to')::date IS NULL OR sc.work_date <= sqlc.narg('date_to'))
  AND (sqlc.narg('status')::text IS NULL
       OR (sqlc.narg('status') = 'CANCELLED' AND sc.cancelled_at IS NOT NULL)
       OR (sqlc.narg('status') = 'IN_PROGRESS' AND sc.cancelled_at IS NULL AND pr.status = 'ACTIVE')
       OR (sqlc.narg('status') = 'COMPLETED' AND sc.cancelled_at IS NULL AND pr.status IN ('ENDED', 'AUTO_ENDED'))
       OR (sqlc.narg('status') = 'MISSED' AND sc.cancelled_at IS NULL AND pr.id IS NULL AND sc.ends_at < @now)
       OR (sqlc.narg('status') = 'SCHEDULED' AND sc.cancelled_at IS NULL AND pr.id IS NULL AND sc.ends_at >= @now));

-- One schedule with its zone and person, the same shape as the list.
-- name: GetPatrolScheduleView :one
SELECT sc.*, z.name AS zone_name, z.type AS zone_type, p.full_name AS personnel_name,
       pr.id AS patrol_id, pr.status AS patrol_status, pr.started_at AS patrol_started_at,
       (SELECT count(*) FROM patrol_scans s WHERE s.patrol_id = pr.id AND s.zone_id = sc.zone_id AND s.outcome = 'SCANNED')::int AS scan_count
FROM patrol_schedules sc
JOIN patrol_zones z ON z.organization_id = sc.organization_id AND z.id = sc.zone_id
JOIN personnels p ON p.id = sc.personnel_id
LEFT JOIN patrol_routes pr ON pr.organization_id = sc.organization_id AND pr.schedule_id = sc.id
WHERE sc.id = @id;
