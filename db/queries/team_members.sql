-- Team members (keanggotaan regu). Everything runs inside database.WithTenant.
--
-- Dates are business dates in the project's timezone and end_date is inclusive: a membership
-- counts on D when start_date <= D and (end_date IS NULL OR end_date >= D).

-- name: InsertTeamMember :one
INSERT INTO team_members (team_code, personnel_id, role, start_date, created_at, updated_at)
VALUES (@team_code, @personnel_id, @role, @start_date, @now, @now)
RETURNING *;

-- Locks the row, so a move, a role change and an end of one membership take turns.
-- name: GetTeamMemberForUpdate :one
SELECT * FROM team_members WHERE id = $1 FOR UPDATE;

-- The one open membership (no end date) of a person in a team. The overlap constraint allows
-- at most one open row per person and team.
-- name: GetOpenTeamMemberForUpdate :one
SELECT * FROM team_members
WHERE team_code = @team_code AND personnel_id = @personnel_id AND end_date IS NULL
FOR UPDATE;

-- name: EndTeamMember :one
UPDATE team_members SET end_date = @end_date, updated_at = @now
WHERE id = @id
RETURNING *;

-- Only for a membership that never took effect (it starts after today): there is no history
-- to keep of a row that was never in force.
-- name: DeleteTeamMember :execrows
DELETE FROM team_members WHERE id = $1;

-- A deleted team ends its memberships: the ones in force end today, the ones that have not
-- started yet go away.
-- name: EndOpenMembershipsOfTeam :execrows
UPDATE team_members SET end_date = @today, updated_at = @now
WHERE team_code = @team_code AND end_date IS NULL AND start_date <= @today;

-- name: DeleteFutureMembershipsOfTeam :execrows
DELETE FROM team_members WHERE team_code = @team_code AND start_date > @today;

-- The members of a team on a date, the DANRU first and then by name.
-- name: ListTeamMembersOn :many
SELECT tm.id, tm.team_code, tm.personnel_id, tm.role, tm.start_date, tm.end_date,
       tm.created_at, tm.updated_at, p.full_name, p.nik
FROM team_members tm
JOIN personnels p ON p.id = tm.personnel_id
WHERE tm.team_code = @team_code
  AND tm.start_date <= @on_date AND (tm.end_date IS NULL OR tm.end_date >= @on_date)
ORDER BY (tm.role <> 'DANRU'), lower(p.full_name), tm.id
LIMIT @page_limit OFFSET @page_offset;

-- name: CountTeamMembersOn :one
SELECT count(*) FROM team_members
WHERE team_code = @team_code
  AND start_date <= @on_date AND (end_date IS NULL OR end_date >= @on_date);

-- name: GetTeamMemberView :one
SELECT tm.id, tm.team_code, tm.personnel_id, tm.role, tm.start_date, tm.end_date,
       tm.created_at, tm.updated_at, p.full_name, p.nik
FROM team_members tm
JOIN personnels p ON p.id = tm.personnel_id
WHERE tm.id = $1;

-- name: GetTeamDanruOn :one
SELECT tm.id, tm.team_code, tm.personnel_id, tm.role, tm.start_date, tm.end_date,
       tm.created_at, tm.updated_at, p.full_name, p.nik
FROM team_members tm
JOIN personnels p ON p.id = tm.personnel_id
WHERE tm.team_code = @team_code AND tm.role = 'DANRU'
  AND tm.start_date <= @on_date AND (tm.end_date IS NULL OR tm.end_date >= @on_date);

-- The teams a person serves in on a date, with the team's and project's names. A deleted
-- team is not one of them.
-- name: ListMembershipsOfPersonnelOn :many
SELECT tm.id, tm.team_code, t.description AS team_name, t.project_code, pr.description AS project_name,
       tm.role, tm.start_date, tm.end_date
FROM team_members tm
JOIN teams t    ON t.organization_id = tm.organization_id AND t.code = tm.team_code
JOIN projects pr ON pr.organization_id = t.organization_id AND pr.code = t.project_code
WHERE tm.personnel_id = @personnel_id AND t.deleted_at IS NULL
  AND tm.start_date <= @on_date AND (tm.end_date IS NULL OR tm.end_date >= @on_date)
ORDER BY tm.team_code;

-- The same for "now": the date is each project's own today, because a business date is the
-- calendar date in the project's timezone, not in UTC (23:30 UTC is already tomorrow in all
-- three Indonesian zones).
-- name: ListCurrentMembershipsOfPersonnel :many
SELECT tm.id, tm.team_code, t.description AS team_name, t.project_code, pr.description AS project_name,
       tm.role, tm.start_date, tm.end_date
FROM team_members tm
JOIN teams t    ON t.organization_id = tm.organization_id AND t.code = tm.team_code
JOIN projects pr ON pr.organization_id = t.organization_id AND pr.code = t.project_code
WHERE tm.personnel_id = @personnel_id AND t.deleted_at IS NULL
  AND tm.start_date <= (sqlc.arg('now')::timestamptz AT TIME ZONE pr.timezone)::date
  AND (tm.end_date IS NULL OR tm.end_date >= (sqlc.arg('now')::timestamptz AT TIME ZONE pr.timezone)::date)
ORDER BY tm.team_code;

-- name: IsMemberOfProjectOn :one
SELECT EXISTS (
    SELECT 1 FROM team_members tm
    JOIN teams t ON t.organization_id = tm.organization_id AND t.code = tm.team_code
    WHERE tm.personnel_id = @personnel_id AND t.project_code = @project_code AND t.deleted_at IS NULL
      AND tm.start_date <= @on_date AND (tm.end_date IS NULL OR tm.end_date >= @on_date));

-- name: IsDanruOfProjectOn :one
SELECT EXISTS (
    SELECT 1 FROM team_members tm
    JOIN teams t ON t.organization_id = tm.organization_id AND t.code = tm.team_code
    WHERE tm.personnel_id = @personnel_id AND t.project_code = @project_code AND t.deleted_at IS NULL
      AND tm.role = 'DANRU'
      AND tm.start_date <= @on_date AND (tm.end_date IS NULL OR tm.end_date >= @on_date));
