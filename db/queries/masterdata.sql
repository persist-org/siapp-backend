-- Master data (Phase 06.1): categories and panic types. Everything runs inside
-- database.WithTenant, so row-level security keeps another organization's rows invisible.
--
-- A delete is a soft delete that also deactivates: the row stays so a delta pull can send it as a
-- tombstone, and its updated_at moves so the pull finds it. Admin reads hide deleted rows; the
-- delta reads below do not.

-- ---------- categories ----------

-- name: ListCategories :many
SELECT * FROM categories
WHERE kind = @kind AND deleted_at IS NULL
  AND (sqlc.narg('active')::boolean IS NULL OR active = sqlc.narg('active'))
  AND (sqlc.narg('q')::text IS NULL OR strpos(lower(categories.name), lower(sqlc.narg('q'))) > 0)
ORDER BY sort_order, name, id
LIMIT @page_limit OFFSET @page_offset;

-- name: CountCategories :one
SELECT count(*) FROM categories
WHERE kind = @kind AND deleted_at IS NULL
  AND (sqlc.narg('active')::boolean IS NULL OR active = sqlc.narg('active'))
  AND (sqlc.narg('q')::text IS NULL OR strpos(lower(categories.name), lower(sqlc.narg('q'))) > 0);

-- name: GetCategory :one
SELECT * FROM categories WHERE id = @id AND deleted_at IS NULL;

-- Locks the row, so two admins' edits of one category are serialised.
-- name: GetCategoryForUpdate :one
SELECT * FROM categories WHERE id = @id AND deleted_at IS NULL FOR UPDATE;

-- A category created without a sort order goes last.
-- name: InsertCategory :one
INSERT INTO categories (kind, name, urgency, business_lines, sort_order, active, created_at, updated_at)
VALUES (@kind, @name, sqlc.narg('urgency'), @business_lines,
        COALESCE(sqlc.narg('sort_order')::int,
                 (SELECT COALESCE(max(c.sort_order) + 1, 0) FROM categories c WHERE c.kind = @kind)),
        @active, @now, @now)
RETURNING *;

-- A NULL narg leaves the column as it is. urgency is never cleared here: it is required for an
-- instruction and absent for a report, and the usecase has checked which this is.
-- name: UpdateCategory :one
UPDATE categories SET
    name           = COALESCE(sqlc.narg('name'), name),
    urgency        = COALESCE(sqlc.narg('urgency'), urgency),
    business_lines = COALESCE(sqlc.narg('business_lines')::text[], business_lines),
    sort_order     = COALESCE(sqlc.narg('sort_order')::int, sort_order),
    active         = COALESCE(sqlc.narg('active')::boolean, active),
    updated_at     = @now
WHERE id = @id AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteCategory :execrows
UPDATE categories SET active = false, deleted_at = @now, updated_at = @now
WHERE id = @id AND deleted_at IS NULL;

-- The full pull of the field app: active categories offered to one of the caller's business
-- lines. A category with no business lines is offered to everyone.
-- name: ListActiveCategoriesForLines :many
SELECT * FROM categories
WHERE kind = @kind AND active AND deleted_at IS NULL
  AND (cardinality(business_lines) = 0 OR business_lines && @lines::text[])
ORDER BY sort_order, name, id;

-- The delta pull: every row changed after `after`, DELETED AND DEACTIVATED ROWS INCLUDED, and NOT
-- filtered by business line. The caller sorts them in Go: a row that is deleted, inactive or no
-- longer offered to the caller's lines (its lines changed) is a tombstone, so a cache that held
-- it drops it; the rest are upserts.
-- name: ListCategoryChanges :many
SELECT * FROM categories
WHERE kind = @kind AND updated_at > @after::timestamptz
ORDER BY updated_at, id;

-- Insert-if-missing, per kind: the defaults go in only while the organization has no category of
-- that kind at all, counting inactive and deleted ones. An admin who renamed or deleted a default
-- is not given it back by the next login. The unique index makes two first logins at once safe.
-- name: InsertDefaultCategories :execrows
INSERT INTO categories (kind, name, urgency, sort_order, created_at, updated_at)
SELECT @kind::category_kind, d.name, NULLIF(d.urgency, '')::urgency, d.ord, @now::timestamptz, @now::timestamptz
FROM (SELECT unnest(@names::text[]) AS name, unnest(@urgencies::text[]) AS urgency,
             unnest(@orders::int[]) AS ord) AS d
WHERE NOT EXISTS (SELECT 1 FROM categories c WHERE c.kind = @kind::category_kind)
ON CONFLICT DO NOTHING;

-- ⚠ Serialises every write that takes a name: two admins creating "Kejadian" at once would both
-- find it free otherwise. The key is built by the caller (organization, kind, lower-cased name)
-- and the lock ends with the transaction.
-- name: LockMasterName :exec
SELECT pg_advisory_xact_lock(hashtextextended(@key::text, 0));

-- Whether another category of the kind, not deleted, active or not, has the name (ignoring case)
-- on a business line that overlaps these. An empty list is every line, so it overlaps all.
-- name: CategoryNameClash :one
SELECT EXISTS (
    SELECT 1 FROM categories
    WHERE kind = @kind AND id <> @id AND deleted_at IS NULL AND lower(name) = lower(@name::text)
      AND (cardinality(business_lines) = 0 OR cardinality(@lines::text[]) = 0
           OR business_lines && @lines::text[])
)::boolean;

-- ---------- panic types ----------

-- name: PanicTypeNameClash :one
SELECT EXISTS (
    SELECT 1 FROM panic_types
    WHERE id <> @id AND deleted_at IS NULL AND lower(name) = lower(@name::text)
)::boolean;

-- The codes that start with a prefix, deleted ones included (a code is never reused), for making
-- a free code from a name.
-- name: ListPanicTypeCodesWithPrefix :many
SELECT code FROM panic_types WHERE code = @prefix::text OR starts_with(code, @prefix::text || '_');

-- name: ListPanicTypes :many
SELECT * FROM panic_types
WHERE deleted_at IS NULL
  AND (sqlc.narg('active')::boolean IS NULL OR active = sqlc.narg('active'))
  AND (sqlc.narg('q')::text IS NULL OR strpos(lower(panic_types.name), lower(sqlc.narg('q'))) > 0
       OR strpos(lower(code), lower(sqlc.narg('q'))) > 0)
ORDER BY sort_order, name, id
LIMIT @page_limit OFFSET @page_offset;

-- name: CountPanicTypes :one
SELECT count(*) FROM panic_types
WHERE deleted_at IS NULL
  AND (sqlc.narg('active')::boolean IS NULL OR active = sqlc.narg('active'))
  AND (sqlc.narg('q')::text IS NULL OR strpos(lower(panic_types.name), lower(sqlc.narg('q'))) > 0
       OR strpos(lower(code), lower(sqlc.narg('q'))) > 0);

-- name: GetPanicType :one
SELECT * FROM panic_types WHERE id = @id AND deleted_at IS NULL;

-- name: GetPanicTypeForUpdate :one
SELECT * FROM panic_types WHERE id = @id AND deleted_at IS NULL FOR UPDATE;

-- name: InsertPanicType :one
INSERT INTO panic_types (code, name, sort_order, active, created_at, updated_at)
VALUES (@code, @name,
        COALESCE(sqlc.narg('sort_order')::int, (SELECT COALESCE(max(p.sort_order) + 1, 0) FROM panic_types p)),
        @active, @now, @now)
RETURNING *;

-- The code is the identity and never changes.
-- name: UpdatePanicType :one
UPDATE panic_types SET
    name       = COALESCE(sqlc.narg('name'), name),
    sort_order = COALESCE(sqlc.narg('sort_order')::int, sort_order),
    active     = COALESCE(sqlc.narg('active')::boolean, active),
    updated_at = @now
WHERE id = @id AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeletePanicType :execrows
UPDATE panic_types SET active = false, deleted_at = @now, updated_at = @now
WHERE id = @id AND deleted_at IS NULL;

-- name: ListActivePanicTypes :many
SELECT * FROM panic_types WHERE active AND deleted_at IS NULL ORDER BY sort_order, name, id;

-- name: ListPanicTypeChanges :many
SELECT * FROM panic_types WHERE updated_at > @after::timestamptz ORDER BY updated_at, id;

-- name: InsertDefaultPanicTypes :execrows
INSERT INTO panic_types (code, name, sort_order, created_at, updated_at)
SELECT d.code, d.name, d.ord, @now::timestamptz, @now::timestamptz
FROM (SELECT unnest(@codes::text[]) AS code, unnest(@names::text[]) AS name,
             unnest(@orders::int[]) AS ord) AS d
-- No conflict target: a default whose code OR name an admin already uses is skipped, never an
-- error that would fail the login seeding it.
ON CONFLICT DO NOTHING;

-- ---------- the caller's business lines ----------

-- The business lines of the projects a person serves in today (each in its own project's time
-- zone, like /auth/me). Categories are offered by these.
-- name: ListBusinessLinesOfPersonnel :many
SELECT DISTINCT pr.business_line
FROM team_members tm
JOIN teams t     ON t.organization_id = tm.organization_id AND t.code = tm.team_code
JOIN projects pr ON pr.organization_id = t.organization_id AND pr.code = t.project_code
CROSS JOIN LATERAL (
    SELECT (sqlc.arg('now')::timestamptz AT TIME ZONE pr.timezone)::date AS d
) AS day
WHERE tm.personnel_id = @personnel_id AND t.deleted_at IS NULL AND pr.deleted_at IS NULL
  AND tm.start_date <= day.d
  AND (tm.end_date IS NULL OR tm.end_date >= day.d)
ORDER BY pr.business_line;

-- ⚠ Whether the caller's business lines may have changed after `since`: a membership of theirs
-- was added, ended or moved, its regu or project was edited or deleted, or a membership started
-- or ended on a day between then and today (in the project's time zone). The delta pull then
-- sends the whole offered set, since no category row changed to tell the app.
-- name: MembershipsChangedSince :one
SELECT EXISTS (
    SELECT 1
    FROM team_members tm
    JOIN teams t     ON t.organization_id = tm.organization_id AND t.code = tm.team_code
    JOIN projects pr ON pr.organization_id = t.organization_id AND pr.code = t.project_code
    CROSS JOIN LATERAL (
        SELECT (sqlc.arg('since')::timestamptz AT TIME ZONE pr.timezone)::date AS then_day,
               (sqlc.arg('now')::timestamptz AT TIME ZONE pr.timezone)::date AS today
    ) AS d
    WHERE tm.personnel_id = @personnel_id
      AND (tm.updated_at > sqlc.arg('since') OR t.updated_at > sqlc.arg('since') OR pr.updated_at > sqlc.arg('since')
           OR (tm.start_date > d.then_day AND tm.start_date <= d.today)
           OR (tm.end_date >= d.then_day AND tm.end_date < d.today))
)::boolean;
