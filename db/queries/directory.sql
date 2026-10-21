-- The Regu screens of the field app (Phase 02.6). Everything runs inside database.WithTenant.
-- "Today" is each project's own business date: the calendar date of @now in the project's
-- timezone (DL-03), computed here so that projects in different zones each get theirs.

-- Teams with their project, client and relationship officer: those of some projects, or one
-- team by code. A deleted team is not listed. Team numbers sort as numbers (S2 before S10).
-- name: ListDirectoryTeams :many
SELECT t.code, t.description AS team_name, t.phone, t.min_personnel, t.project_code,
       pr.description AS project_name, pr.timezone, pr.client_code, c.description AS client_name,
       t.relationship_officer_personnel_id AS ro_id, ro.full_name AS ro_full_name, ro.phone AS ro_phone,
       ro.photo_url AS ro_photo_url
FROM teams t
JOIN projects pr ON pr.organization_id = t.organization_id AND pr.code = t.project_code
JOIN clients c   ON c.organization_id = pr.organization_id AND c.code = pr.client_code
LEFT JOIN personnels ro ON ro.id = t.relationship_officer_personnel_id
WHERE t.deleted_at IS NULL
  AND (sqlc.narg('project_codes')::text[] IS NULL OR t.project_code = ANY(sqlc.narg('project_codes')::text[]))
  AND (sqlc.narg('team_code')::text IS NULL OR t.code = sqlc.narg('team_code'))
ORDER BY t.project_code, length(t.code), t.code;

-- Who serves in some teams today, each with their shift of today if they have one: the DANRU
-- first, then by name.
-- name: ListDirectoryMembersToday :many
SELECT tm.team_code, tm.personnel_id, tm.role, p.full_name, p.phone, p.photo_url, p.position_name,
       sh.id AS shift_id, sh.name AS shift_name, sh.start_minute, sh.end_minute, sh.is_off
FROM team_members tm
JOIN teams t     ON t.organization_id = tm.organization_id AND t.code = tm.team_code
JOIN projects pr ON pr.organization_id = t.organization_id AND pr.code = t.project_code
JOIN personnels p ON p.id = tm.personnel_id
LEFT JOIN schedules s ON s.personnel_id = tm.personnel_id
                     AND s.work_date = (sqlc.arg('now')::timestamptz AT TIME ZONE pr.timezone)::date
LEFT JOIN shifts sh ON sh.id = s.shift_id
WHERE tm.team_code = ANY(@team_codes::text[])
  AND tm.start_date <= (sqlc.arg('now')::timestamptz AT TIME ZONE pr.timezone)::date
  AND (tm.end_date IS NULL OR tm.end_date >= (sqlc.arg('now')::timestamptz AT TIME ZONE pr.timezone)::date)
ORDER BY tm.team_code, (tm.role <> 'DANRU'), lower(p.full_name), tm.personnel_id;
