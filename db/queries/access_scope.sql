-- Field permission scope (Phase 03.2). Everything runs inside database.WithTenant.

-- ⚠ The memberships of a person that grant a permission, on one day: each active membership
-- joined to the grant of its role (NONE and a missing row grant nothing). The day is on_date
-- when the caller names one; otherwise it is "today" in each membership's own project
-- timezone, the same "today" /auth/me uses (23:30 UTC is already tomorrow in all three
-- Indonesian zones). A deleted regu or project has no members. The business line of the
-- project comes with the row, for permissions that belong to one line.
-- name: ListGrantingMemberships :many
SELECT tm.team_code, t.project_code, pr.business_line, rp.scope
FROM team_members tm
JOIN teams t     ON t.organization_id = tm.organization_id AND t.code = tm.team_code
JOIN projects pr ON pr.organization_id = t.organization_id AND pr.code = t.project_code
JOIN role_permissions rp ON rp.organization_id = tm.organization_id AND rp.team_role = tm.role
                        AND rp.permission = @permission AND rp.scope <> 'NONE'
CROSS JOIN LATERAL (
    SELECT COALESCE(sqlc.narg('on_date')::date, (sqlc.arg('now')::timestamptz AT TIME ZONE pr.timezone)::date) AS d
) AS day
WHERE tm.personnel_id = @personnel_id AND t.deleted_at IS NULL AND pr.deleted_at IS NULL
  AND tm.start_date <= day.d
  AND (tm.end_date IS NULL OR tm.end_date >= day.d)
ORDER BY tm.team_code;

-- The regu a PROJECT grant adds: every regu of those projects that is active and not deleted.
-- name: ListActiveTeamsOfProjects :many
SELECT code, project_code FROM teams
WHERE project_code = ANY(@project_codes::text[]) AND deleted_at IS NULL AND active
ORDER BY code;

-- name: GetProjectCodeOfTeam :one
SELECT project_code FROM teams WHERE code = @code AND deleted_at IS NULL;

-- name: GetBusinessLineOfProject :one
SELECT business_line FROM projects WHERE code = @code AND deleted_at IS NULL;

-- The DANRU of a regu on a day. One DANRU per regu per date is a constraint (BR-RULE-02), so at
-- most one row. A DANRU Personalia has deactivated is not someone to send a report to.
-- name: GetDanruOfTeamOn :one
SELECT tm.personnel_id
FROM team_members tm
JOIN personnels p ON p.id = tm.personnel_id AND p.status = 'ACTIVE'
WHERE tm.team_code = @team_code AND tm.role = 'DANRU'
  AND tm.start_date <= @on_date AND (tm.end_date IS NULL OR tm.end_date >= @on_date);

-- The admins who stand in for a missing DANRU: every ORG_ADMIN, and the REGION_ADMINs of the
-- region the project sits under. Only people who are still active.
-- name: ListAdminRecipientsOfRegion :many
SELECT w.personnel_id
FROM web_role_assignments w
JOIN personnels p ON p.id = w.personnel_id AND p.status = 'ACTIVE'
WHERE w.role = 'ORG_ADMIN' OR (w.role = 'REGION_ADMIN' AND w.region_code = @region_code)
ORDER BY w.personnel_id;
