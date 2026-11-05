-- Stakeholder types and the emergency contacts that point at them (Phase 06.2). Everything runs
-- inside database.WithTenant. A delete is a soft delete that also deactivates, as for the other
-- masters; reads below hide deleted rows.

-- name: ListStakeholderTypes :many
SELECT * FROM stakeholder_types
WHERE deleted_at IS NULL
  AND (sqlc.narg('active')::boolean IS NULL OR active = sqlc.narg('active'))
  AND (sqlc.narg('q')::text IS NULL OR strpos(lower(stakeholder_types.name), lower(sqlc.narg('q'))) > 0
       OR strpos(lower(code), lower(sqlc.narg('q'))) > 0)
ORDER BY sort_order, name, id
LIMIT @page_limit OFFSET @page_offset;

-- name: CountStakeholderTypes :one
SELECT count(*) FROM stakeholder_types
WHERE deleted_at IS NULL
  AND (sqlc.narg('active')::boolean IS NULL OR active = sqlc.narg('active'))
  AND (sqlc.narg('q')::text IS NULL OR strpos(lower(stakeholder_types.name), lower(sqlc.narg('q'))) > 0
       OR strpos(lower(code), lower(sqlc.narg('q'))) > 0);

-- name: GetStakeholderType :one
SELECT * FROM stakeholder_types WHERE id = @id AND deleted_at IS NULL;

-- name: GetStakeholderTypeForUpdate :one
SELECT * FROM stakeholder_types WHERE id = @id AND deleted_at IS NULL FOR UPDATE;

-- The types a contact or a panic type is about to point at. FOR SHARE: a delete of one of them in
-- flight holds FOR UPDATE, so this waits for it and then no longer finds the type; and a delete
-- that starts after this waits until the referencing transaction ends, then counts what it wrote.
-- name: LockStakeholderTypesForShare :many
SELECT * FROM stakeholder_types WHERE id = ANY(@ids::bigint[]) AND deleted_at IS NULL ORDER BY id FOR SHARE;

-- Every type that is not deleted, by id, for naming the contacts that point at them.
-- name: ListStakeholderTypesByIDs :many
SELECT * FROM stakeholder_types WHERE id = ANY(@ids::bigint[]) AND deleted_at IS NULL;

-- name: ListActiveStakeholderTypes :many
SELECT * FROM stakeholder_types WHERE active AND deleted_at IS NULL ORDER BY sort_order, name, id;

-- A type created without a sort order goes last.
-- name: InsertStakeholderType :one
INSERT INTO stakeholder_types (code, name, default_phone, sort_order, active, created_at, updated_at)
VALUES (@code, @name, sqlc.narg('default_phone'),
        COALESCE(sqlc.narg('sort_order')::int, (SELECT COALESCE(max(s.sort_order) + 1, 0) FROM stakeholder_types s)),
        @active, @now, @now)
RETURNING *;

-- Writes every editable column: the caller has worked out what the row becomes, so a field the
-- request left out keeps its value and one it cleared (default_phone: null) is cleared.
-- name: UpdateStakeholderType :one
UPDATE stakeholder_types SET name = @name, default_phone = sqlc.narg('default_phone'),
    sort_order = @sort_order, active = @active, updated_at = @now
WHERE id = @id AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteStakeholderType :execrows
UPDATE stakeholder_types SET active = false, deleted_at = @now, updated_at = @now
WHERE id = @id AND deleted_at IS NULL;

-- name: StakeholderTypeNameClash :one
SELECT EXISTS (
    SELECT 1 FROM stakeholder_types
    WHERE id <> @id AND deleted_at IS NULL AND lower(stakeholder_types.name) = lower(@name::text)
)::boolean;

-- See ListPanicTypeCodesWithPrefix.
-- name: ListStakeholderTypeCodesWithPrefix :many
SELECT code FROM stakeholder_types WHERE starts_with(code, @prefix::text);

-- name: CountPanicTypesOfStakeholderType :one
SELECT count(*) FROM panic_types WHERE stakeholder_type_id = @id AND deleted_at IS NULL;

-- Projects whose emergency contacts name the type.
-- name: CountProjectsWithStakeholderType :one
SELECT count(*) FROM projects
WHERE deleted_at IS NULL
  AND jsonb_path_exists(emergency_contacts, '$[*] ? (@.stakeholder_type_id == $id)', jsonb_build_object('id', @id::bigint));

-- Insert-if-missing, per code: a starter type goes in when its code was never used, deleted
-- included, so one an admin deleted or renamed is not given back. Per code, not "while there are
-- none": migration 00021 makes some of these types for organizations that had project contacts
-- before 06.2, and those still get the rest. A name an admin already uses is skipped too (no
-- conflict target), never an error that would fail a login.
-- name: InsertDefaultStakeholderTypes :execrows
INSERT INTO stakeholder_types (code, name, sort_order, created_at, updated_at)
SELECT d.code, d.name, d.ord, @now::timestamptz, @now::timestamptz
FROM (SELECT unnest(@codes::text[]) AS code, unnest(@names::text[]) AS name,
             unnest(@orders::int[]) AS ord) AS d
WHERE NOT EXISTS (SELECT 1 FROM stakeholder_types s WHERE s.code = d.code)
ON CONFLICT DO NOTHING;

-- Who a default panic type calls. Only a panic type that calls nobody yet is linked, so an admin's
-- choice is never overwritten.
-- name: LinkDefaultPanicTypes :execrows
UPDATE panic_types p SET stakeholder_type_id = s.id, updated_at = @now
FROM (SELECT unnest(@panic_codes::text[]) AS panic_code, unnest(@stakeholder_codes::text[]) AS stakeholder_code) m
JOIN stakeholder_types s ON s.code = m.stakeholder_code AND s.deleted_at IS NULL
WHERE p.code = m.panic_code AND p.stakeholder_type_id IS NULL AND p.deleted_at IS NULL;

-- ---------- a project's emergency contacts ----------

-- name: GetProjectContacts :one
SELECT emergency_contacts FROM projects WHERE code = @code AND deleted_at IS NULL;

-- Whether a person serves in a regu of the project today (in the project's time zone).
-- name: IsServingInProject :one
SELECT EXISTS (
    SELECT 1
    FROM team_members tm
    JOIN teams t     ON t.organization_id = tm.organization_id AND t.code = tm.team_code
    JOIN projects pr ON pr.organization_id = t.organization_id AND pr.code = t.project_code
    CROSS JOIN LATERAL (
        SELECT (sqlc.arg('now')::timestamptz AT TIME ZONE pr.timezone)::date AS d
    ) AS day
    WHERE tm.personnel_id = @personnel_id AND pr.code = @project_code
      AND t.deleted_at IS NULL AND pr.deleted_at IS NULL
      AND tm.start_date <= day.d AND (tm.end_date IS NULL OR tm.end_date >= day.d)
)::boolean;
