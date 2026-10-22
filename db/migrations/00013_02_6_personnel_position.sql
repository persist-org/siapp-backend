-- +goose Up

-- Phase 02.6: the person's job title, as Personalia names it, for the Regu screens. It is a
-- cached copy for display only, refreshed with the rest of the person on every login and
-- lookup: Personalia owns positions, and SiAPP's field role is team_members.role.
ALTER TABLE personnels ADD COLUMN position_name TEXT;

-- +goose Down
ALTER TABLE personnels DROP COLUMN position_name;
