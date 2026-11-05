-- +goose Up

-- Phase 06.2: project emergency contacts written before 06.2 named their kind as free text
-- ({type: "Polisi", name, phone}); from 06.2 they point at a stakeholder type
-- ({stakeholder_type_id, name, phone}). This converts the old ones, so a site's own numbers do not
-- vanish from the panic phone book and an admin can save the list they read back.
--
-- Each old type becomes a stakeholder type of its organization: the usual words (Polisi, RS,
-- Pemadam, ...) become the default types POLICE, HOSPITAL and FIRE_DEPARTMENT, with their default
-- names; anything else becomes a type named as it was written, code LEGACY_<hash>. A type that is
-- already there (same code, or same name) is reused. The phones get the cheap part of the API's
-- normalisation (formatting dropped, 0812... -> +62812...); one that is still not a number stays as
-- it was, so nothing is lost, and the admin's next save of the list asks for it to be fixed.
--
-- ⚠ FORCE ROW LEVEL SECURITY applies the tenant policy to the owner too, and a migration has no
-- tenant: lifted for these statements and put back before the transaction commits (as 00008).
ALTER TABLE projects NO FORCE ROW LEVEL SECURITY;
ALTER TABLE stakeholder_types NO FORCE ROW LEVEL SECURITY;
ALTER TABLE panic_types NO FORCE ROW LEVEL SECURITY;

CREATE TEMP TABLE legacy_contact_types ON COMMIT DROP AS
SELECT DISTINCT organization_id, type_name, code, name
FROM (
    SELECT p.organization_id, btrim(c->>'type') AS type_name,
           CASE
               WHEN lower(btrim(c->>'type')) IN ('polisi', 'police', 'polsek', 'polres', 'polda', 'kepolisian')
                   THEN 'POLICE'
               WHEN lower(btrim(c->>'type')) IN ('rs', 'rumah sakit', 'hospital', 'ambulans', 'ambulance', 'puskesmas', 'klinik')
                   THEN 'HOSPITAL'
               WHEN lower(btrim(c->>'type')) IN ('pemadam', 'pemadam kebakaran', 'damkar', 'kebakaran', 'fire', 'fire department')
                   THEN 'FIRE_DEPARTMENT'
               ELSE 'LEGACY_' || upper(substr(md5(lower(btrim(c->>'type'))), 1, 10))
           END AS code
    FROM projects p
    CROSS JOIN LATERAL jsonb_array_elements(p.emergency_contacts) AS c
    WHERE jsonb_typeof(p.emergency_contacts) = 'array'
      AND jsonb_typeof(c) = 'object' AND NOT c ? 'stakeholder_type_id'
      AND btrim(coalesce(c->>'type', '')) <> ''
) t
CROSS JOIN LATERAL (
    SELECT CASE t.code WHEN 'POLICE' THEN 'Polisi' WHEN 'HOSPITAL' THEN 'Rumah Sakit'
                       WHEN 'FIRE_DEPARTMENT' THEN 'Pemadam Kebakaran' ELSE t.type_name END AS name
) n;

-- The types that are not there yet. The default ones take the default sort order.
INSERT INTO stakeholder_types (organization_id, code, name, sort_order, created_at, updated_at)
SELECT DISTINCT ON (l.organization_id, l.code) l.organization_id, l.code, l.name,
       CASE l.code WHEN 'POLICE' THEN 0 WHEN 'HOSPITAL' THEN 1 WHEN 'FIRE_DEPARTMENT' THEN 2 ELSE 100 END,
       now(), now()
FROM legacy_contact_types l
WHERE NOT EXISTS (
    SELECT 1 FROM stakeholder_types s
    WHERE s.organization_id = l.organization_id
      AND (s.code = l.code OR (s.deleted_at IS NULL AND lower(s.name) = lower(l.name))))
ORDER BY l.organization_id, l.code
ON CONFLICT DO NOTHING;

-- Who each default panic type calls. Before 00020 a panic type could not name a stakeholder type,
-- so every link is still empty and there is no admin choice to respect: the default panic types of
-- every organization are linked to the default types that exist now. (EnsureDefaults links the
-- rest when it adds the missing types; it skips an organization that this statement completed.)
UPDATE panic_types p SET stakeholder_type_id = s.id, updated_at = now()
FROM (VALUES ('SECURITY', 'POLICE'), ('MEDICAL', 'HOSPITAL'), ('FIRE', 'FIRE_DEPARTMENT')) AS m(panic_code, stakeholder_code)
JOIN stakeholder_types s ON s.code = m.stakeholder_code AND s.deleted_at IS NULL
WHERE p.code = m.panic_code AND p.organization_id = s.organization_id
  AND p.stakeholder_type_id IS NULL AND p.deleted_at IS NULL;

-- Rewrite every list that still has an old contact, keeping the order. A contact whose type was
-- blank (never valid) is dropped; an already converted one is kept as it is.
UPDATE projects p SET emergency_contacts = converted.contacts
FROM (
    SELECT p2.organization_id, p2.code,
           coalesce(jsonb_agg(
               CASE WHEN e.c ? 'stakeholder_type_id' THEN e.c
                    ELSE jsonb_build_object(
                        'stakeholder_type_id', st.id,
                        'name', btrim(coalesce(e.c->>'name', '')),
                        'phone', CASE
                            WHEN ph.digits ~ '^0[1-9][0-9]{6,12}$' THEN '+62' || substr(ph.digits, 2)
                            WHEN ph.digits ~ '^62[1-9][0-9]{6,12}$' THEN '+' || ph.digits
                            WHEN ph.digits ~ '^\+[1-9][0-9]{7,14}$' THEN ph.digits
                            ELSE e.c->>'phone' END)
               END ORDER BY e.ord) FILTER (WHERE e.c ? 'stakeholder_type_id' OR st.id IS NOT NULL), '[]'::jsonb) AS contacts
    FROM projects p2
    CROSS JOIN LATERAL jsonb_array_elements(p2.emergency_contacts) WITH ORDINALITY AS e(c, ord)
    CROSS JOIN LATERAL (
        SELECT regexp_replace(coalesce(e.c->>'phone', ''), '[\s().-]', '', 'g') AS digits
    ) ph
    LEFT JOIN legacy_contact_types l
           ON l.organization_id = p2.organization_id AND l.type_name = btrim(e.c->>'type')
    LEFT JOIN LATERAL (
        SELECT s.id FROM stakeholder_types s
        WHERE s.organization_id = p2.organization_id AND s.deleted_at IS NULL
          AND (s.code = l.code OR lower(s.name) = lower(l.name))
        ORDER BY (s.code = l.code) DESC, s.id
        LIMIT 1
    ) st ON l.code IS NOT NULL
    WHERE jsonb_typeof(p2.emergency_contacts) = 'array'
      AND EXISTS (SELECT 1 FROM jsonb_array_elements(p2.emergency_contacts) x
                  WHERE jsonb_typeof(x) = 'object' AND NOT x ? 'stakeholder_type_id')
    GROUP BY p2.organization_id, p2.code
) converted
WHERE p.organization_id = converted.organization_id AND p.code = converted.code;

ALTER TABLE projects FORCE ROW LEVEL SECURITY;
ALTER TABLE stakeholder_types FORCE ROW LEVEL SECURITY;
ALTER TABLE panic_types FORCE ROW LEVEL SECURITY;

-- +goose Down
-- A data conversion: there is no old shape to go back to. The stakeholder types it made stay, and
-- so does the shape {stakeholder_type_id, name, phone} of the contacts: code from before 06.2
-- cannot read it: do not roll back past 00021 once a deployment has converted its projects.
SELECT 1;
