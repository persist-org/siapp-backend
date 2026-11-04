-- +goose Up

-- Phase 06.1 review: how a master's name is unique.
--
-- Categories: a name is taken per kind AND per business line, by every category that is not
-- deleted, ACTIVE OR NOT. "Kejadian" for security and "Kejadian" for cleaning may both exist; a
-- second "Kejadian" for security may not, nor one for all lines (an empty business_lines) next to
-- either, since a security person would then be offered two. An inactive category still holds its
-- name: reactivating it must never clash, and the admin list must never show two of one name.
-- The comparison ignores case ("kejadian" is "Kejadian").
--
-- "Overlapping lines" cannot be a unique index (text[] && has no btree operator), so the usecase
-- checks it under a lock on the name. The index below is the backstop for the exact case.
--
-- Panic types: a name is taken by every type that is not deleted, whatever its code.

-- Rows written before this rule may clash. Each clashing row is kept and renamed, never deleted:
-- the row that keeps its name is the active one, then the oldest.
--
-- ⚠ FORCE ROW LEVEL SECURITY applies the tenant policy to the table owner too, and a migration has
-- no tenant: the UPDATEs would see no rows, and the unique indexes below would then fail on the
-- clashes they were meant to clear. FORCE is lifted for the two statements and put back before the
-- migration's transaction commits, so no other session sees the tables without it (as 00008).
ALTER TABLE categories NO FORCE ROW LEVEL SECURITY;
ALTER TABLE panic_types NO FORCE ROW LEVEL SECURITY;
UPDATE categories r SET name = r.name || ' (' || r.id || ')', updated_at = now()
WHERE r.deleted_at IS NULL AND EXISTS (
    SELECT 1 FROM categories o
    WHERE o.organization_id = r.organization_id AND o.kind = r.kind AND o.id <> r.id
      AND o.deleted_at IS NULL AND lower(o.name) = lower(r.name)
      AND (cardinality(o.business_lines) = 0 OR cardinality(r.business_lines) = 0
           OR o.business_lines && r.business_lines)
      AND ((o.active AND NOT r.active) OR (o.active = r.active AND o.id < r.id)));

UPDATE panic_types r SET name = r.name || ' (' || r.code || ')', updated_at = now()
WHERE r.deleted_at IS NULL AND EXISTS (
    SELECT 1 FROM panic_types o
    WHERE o.organization_id = r.organization_id AND o.id <> r.id AND o.deleted_at IS NULL
      AND lower(o.name) = lower(r.name)
      AND ((o.active AND NOT r.active) OR (o.active = r.active AND o.id < r.id)));
ALTER TABLE categories FORCE ROW LEVEL SECURITY;
ALTER TABLE panic_types FORCE ROW LEVEL SECURITY;

DROP INDEX categories_name_uq;
CREATE UNIQUE INDEX categories_name_uq ON categories (organization_id, kind, lower(name), business_lines)
    WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX panic_types_name_uq ON panic_types (organization_id, lower(name))
    WHERE deleted_at IS NULL;

-- +goose Down
-- Fails if two active categories of one kind share a name on different business lines (allowed
-- from here on, not before): rename one first.
DROP INDEX panic_types_name_uq;
DROP INDEX categories_name_uq;
CREATE UNIQUE INDEX categories_name_uq ON categories (organization_id, kind, name) WHERE active;
