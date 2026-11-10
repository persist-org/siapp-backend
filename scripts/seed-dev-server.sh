#!/usr/bin/env bash
# seed-dev-server.sh: seed SiAPP (only SiAPP, never Personalia) with what is needed to test the API
# on the DEV server.
#
#   cd /home/dev-app-workshop/siapp
#   DANRU_UID=.. ANG1_UID=.. ANG2_UID=.. ./scripts/seed-dev-server.sh
#
# What it makes (safe to re-run; rows that exist are left alone):
#   - organization, region OS1 > branch 1K > client 1K-DEMO > project 1K-DEMO-S > regu 1K-DEMO-S1
#     (the project has a GPS point, 150 m radius)
#   - 3 field people (danru + 2 members) and their memberships in the regu
#   - starting shifts, default permissions, first ORG_ADMIN   (SiAPP's own "seed" command)
#   - schedules from 7 days ago to 30 days ahead
#   - ATTENDANCE for the last 6 days: on time, late, absent (mangkir), leave. TODAY IS LEFT EMPTY
#     on purpose, so you can test check-in yourself.
#   - leave categories, a permit quota, 12 days of annual leave per person
#
# Personalia is NOT touched. These accounts must already exist THERE (same NIKs, domain "siapp",
# active, work row in a company listed in GROUP_COMPANY_IDS), or nobody can log in:
#   KPIADMIN01 (admin)   KPIDANRU01 (danru)   KPIANG0001, KPIANG0002 (members)
#
# Settings (environment variables, all optional unless noted):
#   DANRU_UID ANG1_UID ANG2_UID
#                      Personalia m_users.id of KPIDANRU01, KPIANG0001, KPIANG0002.
#                      Needed unless these people are already in SiAPP (after a first login), in
#                      which case they are read from there. A WRONG id makes the first real login
#                      collide on the NIK, so use the real ones.
#   ADMIN_NIK          NIK to make the first ORG_ADMIN (default KPIADMIN01). SiAPP fetches it
#                      from Personalia, so SiAPP must reach Personalia for this step only.
#   PROJECT_DIR        project folder (default: the folder above scripts/)
#   COMPOSE_FILE       default docker-compose.prod.yml
#   ORG_ID             default GROUP_ORGANIZATION_ID from .env, else 1
#   COMPANY_ID         Personalia company id shown next to the people (default: first id of
#                      GROUP_COMPANY_IDS in .env). Display only.
#   EVERY_DAY_SHIFT    true = a 24-hour dev shift on every day (check in at any time, any weekday).
#                      false (default) = the real "5 Hari Kerja" pattern (Day Shift 08-17, Mon-Fri,
#                      OFF at the weekend; there is then no attendance for weekend days).
#   SKIP_ATTENDANCE=1  do not make the attendance rows
#   FORCE=1            allow APP_ENV=production (do not)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="${PROJECT_DIR:-$(cd "$SCRIPT_DIR/.." && pwd)}"
COMPOSE_FILE="${COMPOSE_FILE:-docker-compose.prod.yml}"
EVERY_DAY_SHIFT="${EVERY_DAY_SHIFT:-false}"
ADMIN_NIK="${ADMIN_NIK:-KPIADMIN01}"
SKIP_ATTENDANCE="${SKIP_ATTENDANCE:-0}"

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
warn() { printf '\033[33mWARN: %s\033[0m\n' "$*" >&2; }
die()  { printf '\033[31mERROR: %s\033[0m\n' "$*" >&2; exit 1; }

cd "$PROJECT_DIR"
[ -f "$COMPOSE_FILE" ] || die "$COMPOSE_FILE not found in $PROJECT_DIR (set PROJECT_DIR)"
[ -f .env ] || die ".env not found in $PROJECT_DIR"

dc() { docker compose -f "$COMPOSE_FILE" "$@"; }
envval() { grep -E "^$1=" .env | tail -n1 | cut -d= -f2- || true; }

# ---------- settings ----------
if [ "$(envval APP_ENV)" = "production" ] && [ "${FORCE:-}" != "1" ]; then
  die "APP_ENV=production. This script creates fake people. Set FORCE=1 only if you really mean it."
fi

ORG_ID="${ORG_ID:-$(envval GROUP_ORGANIZATION_ID)}"
ORG_ID="${ORG_ID:-1}"
COMPANY_ID="${COMPANY_ID:-$(envval GROUP_COMPANY_IDS | cut -d, -f1)}"
[[ "$ORG_ID" =~ ^[0-9]+$ ]]     || die "ORG_ID must be a number, got '$ORG_ID'"
[[ "$COMPANY_ID" =~ ^[0-9]+$ ]] || die "COMPANY_ID must be a number (set it, or fix GROUP_COMPANY_IDS in .env), got '$COMPANY_ID'"
[[ "$ADMIN_NIK" =~ ^[A-Za-z0-9]+$ ]] || die "ADMIN_NIK must be letters and digits only"
case "$EVERY_DAY_SHIFT" in true|false) ;; *) die "EVERY_DAY_SHIFT must be true or false" ;; esac

echo "project:  $PROJECT_DIR"
echo "compose:  $COMPOSE_FILE"
echo "org id:   $ORG_ID    company id: $COMPANY_ID    24h dev shift: $EVERY_DAY_SHIFT"

# ---------- preflight ----------
say "Checking the SiAPP database"
dc exec -T postgres pg_isready -U siapp_owner -d siapp >/dev/null || die "postgres is not ready. Start it: docker compose -f $COMPOSE_FILE up -d postgres"
dc exec -T postgres psql -U siapp_owner -d siapp -tAc "SELECT 1 FROM goose_db_version LIMIT 1" >/dev/null 2>&1 \
  || die "the database has no migrations yet. Run: docker compose -f $COMPOSE_FILE up -d --build  (migrate must have exited 0)"
echo "ok"

# ---------- Personalia user ids of the three field people ----------
if [ -z "${DANRU_UID:-}" ] || [ -z "${ANG1_UID:-}" ] || [ -z "${ANG2_UID:-}" ]; then
  # Not given: use the ones SiAPP already knows (the people have signed in, or an earlier run).
  KNOWN="$(dc exec -T postgres psql -U siapp_owner -d siapp -tA -F ' ' -v org="$ORG_ID" 2>/dev/null <<'SQL' || true
SELECT set_config('app.current_org', :'org', false) \gset
SELECT nik, personel_user_id FROM personnels WHERE nik IN ('KPIDANRU01', 'KPIANG0001', 'KPIANG0002');
SQL
)"
  known_uid() { printf '%s\n' "$KNOWN" | awk -v n="$1" '$1==n {print $2}'; }
  DANRU_UID="${DANRU_UID:-$(known_uid KPIDANRU01)}"
  ANG1_UID="${ANG1_UID:-$(known_uid KPIANG0001)}"
  ANG2_UID="${ANG2_UID:-$(known_uid KPIANG0002)}"
fi
for v in DANRU_UID ANG1_UID ANG2_UID; do
  [[ "${!v:-}" =~ ^[0-9]+$ ]] || die "$v is missing. Pass the Personalia m_users.id, e.g. $v=51977 (find it in Personalia: SELECT id, employee_nik FROM m_users WHERE employee_nik IN ('KPIDANRU01','KPIANG0001','KPIANG0002');)"
done
echo "personalia user ids: danru=$DANRU_UID anggota1=$ANG1_UID anggota2=$ANG2_UID"

PSQL() {
  dc exec -T postgres psql -U siapp_owner -d siapp -v ON_ERROR_STOP=1 -q \
    -v org="$ORG_ID" -v company="$COMPANY_ID" -v every_day="$EVERY_DAY_SHIFT" \
    -v attendance="$([ "$SKIP_ATTENDANCE" = "1" ] && echo false || echo true)" \
    -v danru_uid="$DANRU_UID" -v ang1_uid="$ANG1_UID" -v ang2_uid="$ANG2_UID" "$@"
}

# ---------- 1. hierarchy + people ----------
say "1/5 Organization, hierarchy, people, memberships"
PSQL <<'SQL'
SELECT set_config('app.current_org', :'org', false) \gset
BEGIN;

INSERT INTO organizations (id, name) VALUES (:org, 'KPI Group') ON CONFLICT (id) DO NOTHING;

INSERT INTO regions (code, description) VALUES ('OS1', 'Koordinator Wilayah 1') ON CONFLICT DO NOTHING;
INSERT INTO branches (code, region_code, company_letter, description)
  VALUES ('1K', 'OS1', 'K', 'KPI Cabang 1') ON CONFLICT DO NOTHING;
INSERT INTO clients (code, branch_code, description, company_prefix)
  VALUES ('1K-DEMO', '1K', 'Klien Demo', 'DEMO') ON CONFLICT DO NOTHING;
-- The GPS point must be set, or check-in answers PROJECT_NOT_CONFIGURED (Monas, Jakarta).
INSERT INTO projects (code, client_code, description, business_line, timezone,
                      latitude, longitude, attendance_radius_meters, next_team_number)
  VALUES ('1K-DEMO-S', '1K-DEMO', 'Proyek Demo Security', 'security', 'Asia/Jakarta',
          -6.175392, 106.827153, 150, 2) ON CONFLICT DO NOTHING;
INSERT INTO teams (code, project_code, description)
  VALUES ('1K-DEMO-S1', '1K-DEMO-S', 'Regu 1') ON CONFLICT DO NOTHING;

-- (The admin is not here: "seed grant-admin" fetches and stores them in step 4.)
INSERT INTO personnels (personel_user_id, nik, full_name, email, company_id, company_name, position_name) VALUES
  (:danru_uid, 'KPIDANRU01', 'Danru KPI',     'danru.kpi@siapp.test',    :company, 'PT KPI', 'Danru'),
  (:ang1_uid,  'KPIANG0001', 'Anggota KPI 1', 'anggota1.kpi@siapp.test', :company, 'PT KPI', 'Anggota'),
  (:ang2_uid,  'KPIANG0002', 'Anggota KPI 2', 'anggota2.kpi@siapp.test', :company, 'PT KPI', 'Anggota')
ON CONFLICT DO NOTHING;

-- Memberships start a week back (project timezone), so recent dates still resolve to the regu.
INSERT INTO team_members (team_code, personnel_id, role, start_date)
SELECT '1K-DEMO-S1', p.id,
       CASE p.nik WHEN 'KPIDANRU01' THEN 'DANRU'::team_role ELSE 'MEMBER'::team_role END,
       ((now() AT TIME ZONE 'Asia/Jakarta')::date - 7)
FROM personnels p
WHERE p.nik IN ('KPIDANRU01', 'KPIANG0001', 'KPIANG0002')
  AND NOT EXISTS (SELECT 1 FROM team_members tm
                  WHERE tm.team_code = '1K-DEMO-S1' AND tm.personnel_id = p.id AND tm.end_date IS NULL);

COMMIT;
SQL
echo "ok"

# ---------- 2-3. SiAPP seed commands ----------
SEED() { dc run --rm -T --entrypoint /seed api "$@"; }

say "2/5 Starting shifts and shift group (seed shifts)"
SEED shifts

say "3/5 Default field permissions (seed permissions)"
SEED permissions

# ---------- 4. first admin ----------
say "4/5 First ORG_ADMIN ($ADMIN_NIK)"
if SEED grant-admin --nik "$ADMIN_NIK"; then
  echo "ok"
else
  warn "grant-admin failed. It asks Personalia for the NIK, so SiAPP must reach Personalia first"
  warn "(PERSONEL_BASE_URL / key in .env) and $ADMIN_NIK must exist there. Fix that and re-run (safe)."
fi

# ---------- 5. schedules, leave, attendance ----------
say "5/5 Schedules, leave data and attendance"
PSQL <<'SQL'
SELECT set_config('app.current_org', :'org', false) \gset
BEGIN;

\if :every_day
  -- A 24-hour dev shift (00:00-23:59, 12 h tolerance): check in works at any time of day.
  INSERT INTO shifts (name, start_minute, end_minute, tolerance_minutes)
  SELECT 'Dev 24 Jam', 0, 1439, 720
  WHERE NOT EXISTS (SELECT 1 FROM shifts WHERE lower(name) = 'dev 24 jam' AND deleted_at IS NULL);

  INSERT INTO personnel_shift_assignments (personnel_id, schedule_type, effective_date, generated_until)
  SELECT p.id, 'FLEXIBLE', ((now() AT TIME ZONE 'Asia/Jakarta')::date - 7), ((now() AT TIME ZONE 'Asia/Jakarta')::date + 30)
  FROM personnels p WHERE p.nik IN ('KPIDANRU01', 'KPIANG0001', 'KPIANG0002')
  ON CONFLICT DO NOTHING;

  INSERT INTO schedules (personnel_id, work_date, shift_id)
  SELECT p.id, d::date, (SELECT id FROM shifts WHERE lower(name) = 'dev 24 jam' AND deleted_at IS NULL)
  FROM personnels p
  CROSS JOIN generate_series((now() AT TIME ZONE 'Asia/Jakarta')::date - 7,
                             (now() AT TIME ZONE 'Asia/Jakarta')::date + 30, interval '1 day') d
  WHERE p.nik IN ('KPIDANRU01', 'KPIANG0001', 'KPIANG0002')
  ON CONFLICT DO NOTHING;
\else
  -- The real pattern: group "5 Hari Kerja" (Day Shift 08:00-17:00 Monday to Friday, OFF at the weekend).
  INSERT INTO personnel_shift_assignments (personnel_id, schedule_type, shift_group_id, effective_date, generated_until)
  SELECT p.id, 'FIXED', g.id, ((now() AT TIME ZONE 'Asia/Jakarta')::date - 7), ((now() AT TIME ZONE 'Asia/Jakarta')::date + 30)
  FROM personnels p
  JOIN shift_groups g ON lower(g.name) = lower('5 Hari Kerja') AND g.deleted_at IS NULL
  WHERE p.nik IN ('KPIDANRU01', 'KPIANG0001', 'KPIANG0002')
  ON CONFLICT DO NOTHING;

  INSERT INTO schedules (personnel_id, work_date, shift_id, shift_group_id)
  SELECT p.id, d::date, gd.shift_id, g.id
  FROM personnels p
  JOIN shift_groups g ON lower(g.name) = lower('5 Hari Kerja') AND g.deleted_at IS NULL
  CROSS JOIN generate_series((now() AT TIME ZONE 'Asia/Jakarta')::date - 7,
                             (now() AT TIME ZONE 'Asia/Jakarta')::date + 30, interval '1 day') d
  JOIN shift_group_days gd ON gd.shift_group_id = g.id AND gd.weekday = extract(isodow FROM d)::int
  WHERE p.nik IN ('KPIDANRU01', 'KPIANG0001', 'KPIANG0002')
  ON CONFLICT DO NOTHING;
\endif

-- Leave: an annual category (with a ledger), sick (no quota), a permit (12 days a year by default).
INSERT INTO leave_categories (code, name, type, is_annual, has_quota, makes_leave_days, sort_order) VALUES
  ('ANNUAL', 'Cuti Tahunan', 'LEAVE',  true,  true,  true, 1),
  ('SICK',   'Sakit',        'SICK',   false, false, true, 2),
  ('PERMIT', 'Izin',         'PERMIT', false, true,  true, 3)
ON CONFLICT (organization_id, code) DO NOTHING;

INSERT INTO leave_category_quotas (category_id, client_code, quota_days)
SELECT id, NULL::text, 12 FROM leave_categories WHERE code = 'PERMIT'
ON CONFLICT ON CONSTRAINT leave_category_quotas_uq DO NOTHING;

-- 12 days of annual leave for each field person (once).
INSERT INTO leave_ledger (personnel_id, kind, days, balance_after, note)
SELECT p.id, 'GRANT'::leave_ledger_kind, 12, 12, 'dev seed'
FROM personnels p
WHERE p.nik IN ('KPIDANRU01', 'KPIANG0001', 'KPIANG0002')
  AND NOT EXISTS (SELECT 1 FROM leave_ledger l WHERE l.personnel_id = p.id);

-- Attendance for the last 6 days, one row per person and scheduled working day (never an OFF day,
-- never today). The shift times and timezone are copied into the row, as the app does.
--   danru:    always on time
--   anggota1: late every third day of the month
--   anggota2: by day of month: 0 absent (mangkir), 1 leave, 2 late, otherwise on time
-- "on time" checks in 5 minutes after the shift starts, "late" 20 minutes after; the lateness is
-- only counted when it is beyond the shift's tolerance (as the app does). Check-out is 2 minutes
-- after the shift ends. Rows are marked SYNCED because no outbox row is made for seeded data.
\if :attendance
WITH base AS (
  SELECT p.id AS pid, p.nik, sc.work_date, sc.shift_id, sh.tolerance_minutes,
         ((sc.work_date::timestamp + sh.start_minute * interval '1 minute') AT TIME ZONE 'Asia/Jakarta') AS starts_at,
         ((sc.work_date::timestamp + (sh.start_minute + CASE WHEN sh.end_minute > sh.start_minute
                                                             THEN sh.end_minute - sh.start_minute
                                                             ELSE sh.end_minute + 1440 - sh.start_minute END)
                                      * interval '1 minute') AT TIME ZONE 'Asia/Jakarta') AS ends_at,
         extract(day FROM sc.work_date)::int AS dn
  FROM team_members tm
  JOIN personnels p ON p.id = tm.personnel_id
  JOIN schedules sc ON sc.personnel_id = p.id
   AND sc.work_date BETWEEN (now() AT TIME ZONE 'Asia/Jakarta')::date - 6
                        AND (now() AT TIME ZONE 'Asia/Jakarta')::date - 1
  JOIN shifts sh ON sh.id = sc.shift_id AND NOT sh.is_off
  WHERE tm.team_code = '1K-DEMO-S1' AND tm.end_date IS NULL
    AND tm.start_date <= sc.work_date
), kind AS (
  SELECT base.*,
         CASE
           WHEN nik = 'KPIDANRU01' THEN 'ONTIME'
           WHEN nik = 'KPIANG0001' THEN CASE WHEN dn % 3 = 0 THEN 'LATE' ELSE 'ONTIME' END
           ELSE CASE dn % 5 WHEN 0 THEN 'ABSENT' WHEN 1 THEN 'LEAVE' WHEN 2 THEN 'LATE' ELSE 'ONTIME' END
         END AS k
  FROM base
), timed AS (
  SELECT kind.*, CASE WHEN k = 'LATE' THEN 20 ELSE 5 END AS off_min FROM kind
)
INSERT INTO attendances (
    personnel_id, team_code, project_code, work_date, shift_id, shift_starts_at, shift_ends_at, timezone, status,
    check_in_at, check_in_method, check_in_latitude, check_in_longitude, check_in_accuracy_meters,
    check_in_distance_meters, check_in_within_area, late_minutes,
    check_out_at, check_out_method, check_out_latitude, check_out_longitude, check_out_accuracy_meters,
    check_out_distance_meters, check_out_within_area,
    marked_absent_by_id, absent_reason, personel_sync_status, personel_synced_at)
SELECT t.pid, '1K-DEMO-S1', '1K-DEMO-S', t.work_date, t.shift_id, t.starts_at, t.ends_at, 'Asia/Jakarta',
       (CASE t.k WHEN 'LEAVE' THEN 'LEAVE' WHEN 'ABSENT' THEN 'ABSENT' ELSE 'PRESENT' END)::attendance_status,
       CASE WHEN t.k IN ('ONTIME', 'LATE') THEN t.starts_at + t.off_min * interval '1 minute' END,
       CASE WHEN t.k IN ('ONTIME', 'LATE') THEN 'SELFIE'::attendance_method END,
       CASE WHEN t.k IN ('ONTIME', 'LATE') THEN -6.175392 END,
       CASE WHEN t.k IN ('ONTIME', 'LATE') THEN 106.827153 END,
       CASE WHEN t.k IN ('ONTIME', 'LATE') THEN 10 END,
       CASE WHEN t.k IN ('ONTIME', 'LATE') THEN 25 END,
       CASE WHEN t.k IN ('ONTIME', 'LATE') THEN true END,
       CASE WHEN t.k = 'LATE' AND t.off_min > t.tolerance_minutes THEN t.off_min ELSE 0 END,
       CASE WHEN t.k IN ('ONTIME', 'LATE') THEN t.ends_at + interval '2 minutes' END,
       CASE WHEN t.k IN ('ONTIME', 'LATE') THEN 'SELFIE'::attendance_method END,
       CASE WHEN t.k IN ('ONTIME', 'LATE') THEN -6.175392 END,
       CASE WHEN t.k IN ('ONTIME', 'LATE') THEN 106.827153 END,
       CASE WHEN t.k IN ('ONTIME', 'LATE') THEN 10 END,
       CASE WHEN t.k IN ('ONTIME', 'LATE') THEN 25 END,
       CASE WHEN t.k IN ('ONTIME', 'LATE') THEN true END,
       CASE WHEN t.k = 'ABSENT' THEN (SELECT id FROM personnels WHERE nik = 'KPIDANRU01') END,
       CASE WHEN t.k = 'ABSENT' THEN 'Tidak hadir tanpa kabar (dev seed)' END,
       'SYNCED'::personel_sync_status, now()
FROM timed t
ON CONFLICT ON CONSTRAINT attendances_instance_uq DO NOTHING;
\endif

COMMIT;
SQL
echo "ok"

# ---------- summary ----------
say "Result"
PSQL <<'SQL'
SELECT set_config('app.current_org', :'org', false) \gset
\pset footer off
\echo Regu members
SELECT tm.team_code, tm.role, p.nik, p.full_name,
       (SELECT count(*) FROM schedules s WHERE s.personnel_id = p.id) AS schedule_days
FROM team_members tm JOIN personnels p ON p.id = tm.personnel_id
ORDER BY (tm.role <> 'DANRU'), p.nik;
\echo Attendance (last 6 days)
SELECT p.nik, a.status, count(*) AS days, coalesce(sum(a.late_minutes), 0) AS late_minutes
FROM attendances a JOIN personnels p ON p.id = a.personnel_id
GROUP BY p.nik, a.status ORDER BY p.nik, a.status;
SQL

cat <<EOF

Test login body (POST /api/v1/auth/login). The password is whatever the account has in Personalia:
  { "identifier": "danru.kpi@siapp.test", "password": "<password>",
    "client": "MOBILE", "device_token": "my-test-phone-001", "remember_me": true }

People: danru KPIDANRU01, members KPIANG0001 and KPIANG0002, admin $ADMIN_NIK (ORG_ADMIN, if step 4 worked)
Regu 1K-DEMO-S1 at project 1K-DEMO-S, GPS -6.175392, 106.827153 (radius 150 m).
Today has no attendance rows: check in as a member to test it.
EOF
