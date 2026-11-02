-- Realtime rooms and Personel events (Phase 05.3). Everything runs inside database.WithTenant.

-- Is the person a member of the regu today? "Today" is the calendar date in the regu's project
-- timezone, like every business date (23:30 UTC is already tomorrow in Indonesia). Decides who
-- may join team.{code}.panic.
-- name: IsCurrentTeamMember :one
SELECT EXISTS (
    SELECT 1
    FROM team_members tm
    JOIN teams t    ON t.organization_id = tm.organization_id AND t.code = tm.team_code
    JOIN projects p ON p.organization_id = t.organization_id AND p.code = t.project_code
    WHERE tm.team_code = @team_code AND tm.personnel_id = @personnel_id
      AND tm.start_date <= timezone(p.timezone, @now::timestamptz)::date
      AND (tm.end_date IS NULL OR tm.end_date >= timezone(p.timezone, @now::timestamptz)::date)
) AS is_member;

-- Does the project exist? An admin may join the rooms of a project that is in their scope, and
-- a project that is not there answers like one that is out of scope.
-- name: ProjectExists :one
SELECT EXISTS (SELECT 1 FROM projects WHERE code = @code AND deleted_at IS NULL) AS present;

-- A person who left Personel leaves their regu. The memberships in force end today (in each
-- regu's own timezone, also one that already ends on a later date); the ones that have not
-- started yet never took effect and go away. Both return the regu codes for the audit trail.
-- name: EndMembershipsOfPersonEverywhere :many
UPDATE team_members tm SET end_date = timezone(p.timezone, @now::timestamptz)::date, updated_at = @now
FROM teams t, projects p
WHERE tm.personnel_id = @personnel_id
  AND t.organization_id = tm.organization_id AND t.code = tm.team_code
  AND p.organization_id = t.organization_id AND p.code = t.project_code
  AND tm.start_date <= timezone(p.timezone, @now::timestamptz)::date
  AND (tm.end_date IS NULL OR tm.end_date > timezone(p.timezone, @now::timestamptz)::date)
RETURNING tm.team_code;

-- name: DeletePlannedMembershipsOfPersonEverywhere :many
DELETE FROM team_members tm
USING teams t, projects p
WHERE tm.personnel_id = @personnel_id
  AND t.organization_id = tm.organization_id AND t.code = tm.team_code
  AND p.organization_id = t.organization_id AND p.code = t.project_code
  AND tm.start_date > timezone(p.timezone, @now::timestamptz)::date
RETURNING tm.team_code;

-- position.changed: Personel names the new position.
-- name: UpdatePersonnelPosition :exec
UPDATE personnels SET position_name = @position_name, updated_at = @now WHERE id = @id;
