-- Leave, permit and sick requests (Phase 07.5). Everything runs inside database.WithTenant.

-- ---------- categories ----------

-- name: ListLeaveCategories :many
SELECT * FROM leave_categories
WHERE (sqlc.narg('type')::leave_category_type IS NULL OR type = sqlc.narg('type'))
  AND (sqlc.narg('active')::boolean IS NULL OR active = sqlc.narg('active'))
  AND (sqlc.narg('q')::text IS NULL OR strpos(lower(name), lower(sqlc.narg('q'))) > 0
       OR strpos(lower(code), lower(sqlc.narg('q'))) > 0)
ORDER BY sort_order, id
LIMIT @page_limit OFFSET @page_offset;

-- name: CountLeaveCategories :one
SELECT count(*) FROM leave_categories
WHERE (sqlc.narg('type')::leave_category_type IS NULL OR type = sqlc.narg('type'))
  AND (sqlc.narg('active')::boolean IS NULL OR active = sqlc.narg('active'))
  AND (sqlc.narg('q')::text IS NULL OR strpos(lower(name), lower(sqlc.narg('q'))) > 0
       OR strpos(lower(code), lower(sqlc.narg('q'))) > 0);

-- The categories the field offers: active ones, optionally of one type.
-- name: ListActiveLeaveCategories :many
SELECT * FROM leave_categories
WHERE active AND (sqlc.narg('type')::leave_category_type IS NULL OR type = sqlc.narg('type'))
ORDER BY sort_order, id;

-- The delta pull: every row changed after the watermark, deactivated ones included (they go out
-- as tombstones, DL-57).
-- name: ListLeaveCategoryChanges :many
SELECT * FROM leave_categories
WHERE updated_at > @after::timestamptz AND (sqlc.narg('type')::leave_category_type IS NULL OR type = sqlc.narg('type'))
ORDER BY updated_at, id;

-- name: GetLeaveCategory :one
SELECT * FROM leave_categories WHERE code = @code;

-- Locked, so two admins' edits of one category take turns.
-- name: GetLeaveCategoryForUpdate :one
SELECT * FROM leave_categories WHERE code = @code FOR UPDATE;

-- name: InsertLeaveCategory :one
INSERT INTO leave_categories (
    code, name, type, is_annual, skips_day_check, requires_time_range, requires_change_day,
    makes_leave_days, excuses_lateness, has_quota, sort_order, active, created_at, updated_at)
VALUES (
    @code, @name, @type, @is_annual, @skips_day_check, @requires_time_range, @requires_change_day,
    @makes_leave_days, @excuses_lateness, @has_quota,
    COALESCE(sqlc.narg('sort_order')::int, (SELECT COALESCE(max(c.sort_order) + 1, 0) FROM leave_categories c)),
    @active, @now, @now)
RETURNING *;

-- A NULL narg leaves the column as it is.
-- name: UpdateLeaveCategory :one
UPDATE leave_categories SET
    name                = COALESCE(sqlc.narg('name'), name),
    skips_day_check     = COALESCE(sqlc.narg('skips_day_check'), skips_day_check),
    requires_time_range = COALESCE(sqlc.narg('requires_time_range'), requires_time_range),
    requires_change_day = COALESCE(sqlc.narg('requires_change_day'), requires_change_day),
    makes_leave_days    = COALESCE(sqlc.narg('makes_leave_days'), makes_leave_days),
    excuses_lateness    = COALESCE(sqlc.narg('excuses_lateness'), excuses_lateness),
    has_quota           = COALESCE(sqlc.narg('has_quota'), has_quota),
    sort_order          = COALESCE(sqlc.narg('sort_order'), sort_order),
    active              = COALESCE(sqlc.narg('active'), active),
    updated_at          = @now
WHERE id = @id
RETURNING *;

-- Seeds the defaults of an organization. Insert-if-missing by code: a default an admin renamed or
-- deactivated is not given back, and the usual call (every login, all rows present) writes nothing.
-- name: InsertDefaultLeaveCategories :execrows
INSERT INTO leave_categories (
    code, name, type, is_annual, skips_day_check, requires_time_range, requires_change_day,
    makes_leave_days, excuses_lateness, has_quota, sort_order, created_at, updated_at)
SELECT d.code, d.name, d.type::leave_category_type, d.is_annual, d.skips_day_check, d.requires_time_range,
       d.requires_change_day, d.makes_leave_days, d.excuses_lateness, d.has_quota, d.ord,
       @now::timestamptz, @now::timestamptz
FROM (SELECT unnest(@codes::text[]) AS code, unnest(@names::text[]) AS name, unnest(@types::text[]) AS type,
             unnest(@is_annual::boolean[]) AS is_annual, unnest(@skips_day_check::boolean[]) AS skips_day_check,
             unnest(@requires_time_range::boolean[]) AS requires_time_range,
             unnest(@requires_change_day::boolean[]) AS requires_change_day,
             unnest(@makes_leave_days::boolean[]) AS makes_leave_days,
             unnest(@excuses_lateness::boolean[]) AS excuses_lateness, unnest(@has_quota::boolean[]) AS has_quota,
             unnest(@orders::int[]) AS ord) AS d
WHERE NOT EXISTS (SELECT 1 FROM leave_categories c WHERE c.code = d.code)
ON CONFLICT DO NOTHING;

-- ---------- quotas ----------

-- name: ListLeaveCategoryQuotas :many
SELECT * FROM leave_category_quotas WHERE category_id = @category_id ORDER BY client_code NULLS FIRST;

-- A client's row wins over the default; no row at all is 0 days.
-- name: GetEffectiveLeaveQuota :one
SELECT quota_days FROM leave_category_quotas
WHERE category_id = @category_id AND (client_code = sqlc.narg('client_code')::text OR client_code IS NULL)
ORDER BY client_code NULLS LAST
LIMIT 1;

-- name: DeleteLeaveCategoryQuotas :exec
DELETE FROM leave_category_quotas WHERE category_id = @category_id;

-- name: InsertLeaveCategoryQuota :exec
INSERT INTO leave_category_quotas (category_id, client_code, quota_days, created_at, updated_at)
VALUES (@category_id, sqlc.narg('client_code'), @quota_days, @now, @now);

-- name: UpsertLeaveClientSetting :exec
INSERT INTO leave_client_settings (client_code, requests_enabled, updated_at)
VALUES (@client_code, @requests_enabled, @now)
ON CONFLICT (organization_id, client_code) DO UPDATE SET requests_enabled = EXCLUDED.requests_enabled, updated_at = EXCLUDED.updated_at;

-- name: GetLeaveClientSetting :one
SELECT requests_enabled FROM leave_client_settings WHERE client_code = @client_code;

-- name: LeaveClientExists :one
SELECT EXISTS (SELECT 1 FROM clients WHERE code = @code AND deleted_at IS NULL);

-- ---------- ledger ----------

-- The balance is the last row's: 0 for a person with no row.
-- name: GetLeaveBalance :one
SELECT COALESCE((SELECT balance_after FROM leave_ledger WHERE personnel_id = @personnel_id ORDER BY id DESC LIMIT 1), 0)::int AS balance;

-- name: InsertLeaveLedger :one
INSERT INTO leave_ledger (personnel_id, kind, days, balance_after, leave_request_id, note, created_by_id, created_at)
VALUES (@personnel_id, @kind, @days, @balance_after, sqlc.narg('leave_request_id'), @note, sqlc.narg('created_by_id'), @now)
RETURNING *;

-- name: ListLeaveLedger :many
SELECT * FROM leave_ledger WHERE personnel_id = @personnel_id ORDER BY id DESC LIMIT @page_limit OFFSET @page_offset;

-- name: CountLeaveLedger :one
SELECT count(*) FROM leave_ledger WHERE personnel_id = @personnel_id;

-- The ledger row of one kind of a request: how a refund knows it was already made.
-- name: GetLeaveLedgerOfRequest :one
SELECT * FROM leave_ledger WHERE leave_request_id = @leave_request_id AND kind = @kind;
