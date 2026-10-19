-- +goose Up

-- Personalia's NIK column (m_employee_work / m_users) is VARCHAR(100), and the Personel client
-- accepts NIKs up to 100 characters. 32 here turned a longer NIK into "value too long" and a
-- failed login. Widening a VARCHAR is a catalog-only change in Postgres: no table rewrite.
ALTER TABLE personnels ALTER COLUMN nik TYPE VARCHAR(100);

-- +goose Down
-- Refuses (rather than truncates) if a longer NIK has been stored since; that row must be
-- dealt with by hand before going back.
ALTER TABLE personnels ALTER COLUMN nik TYPE VARCHAR(32);
