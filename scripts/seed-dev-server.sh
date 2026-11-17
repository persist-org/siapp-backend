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
#   - patrol zones in 1K-DEMO-S (50 m radius): PATROL "Pos Gerbang Utama" (3 questions) and "Pos Belakang",
#     GUARD "Pos Jaga Depan" (regu 1). Their QR secrets are made here; print or read them from patrol_zones.
#   - patrol CHECKPOINTS in 1K-DEMO-S (30 m radius), each with its own QR secret and checklist, and the
#     zones' order of them: "Pos Gerbang Utama" = Gerbang Utama, Pagar Timur, Pintu Belakang;
#     "Pos Belakang" = Pintu Belakang (shared with the first), Ruang Panel Listrik; GUARD "Pos Jaga Depan"
#     = Pos Jaga, Gerbang Utama (shared with a PATROL zone). The QR text of each is printed at the end
#     of step 6 ("PERSIST-CP:..."): turn it into a QR code to scan. Filled only where empty, so an
#     admin's edits survive a re-run.
#   - patrol routes (2 ended + 1 running), scans of the last 3 days in every state (SUBMITTED waiting
#     for the danru, APPROVED, DECLINED), and zone check-ins (closed + one open). These scans are of the
#     old kind (a zone, no checkpoint) and answer the old zone checklist: they predate the checkpoints
#     and Phase 10.10 removes them.
#   - leave requests in every state (PENDING, APPROVED, REJECTED, CANCELLED) with the ledger and counters,
#     one shift swap, GPS points + last locations, the inbox notifications, and the master data
#     (panic / stakeholder types, report + instruction categories, app links, a letterhead)
#   Not seeded on purpose: media (needs real files), devices / sessions / device bindings (made by
#   login), audit_logs, idempotency_keys and personel_outbox (made by the app as it runs).
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
#   ASSUME_YES=1       do not ask before wiping the SiAPP database (for scripts / CI only)
#   SKIP_WIPE=1        do not wipe: seed on top of what is there (the old behaviour)
#
# WARNING: unless SKIP_WIPE=1, the script FIRST DELETES ALL DATA in the SiAPP database (every table
# except the goose migration history), after asking you to type the database name to confirm.
# Personalia is never touched. Take a backup first if anything in there matters.

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

# ---------- 0. wipe the SiAPP database (asks first) ----------
# After the Personalia ids above are known (they may have been read from this very database).
# Everything is removed except the goose migration history, so the schema stays and no migrate
# run is needed. The seed below then rebuilds what it needs.
if [ "${SKIP_WIPE:-0}" != "1" ]; then
  say "0/8 Wipe the SiAPP database"
  WIPE_TABLES="$(dc exec -T postgres psql -U siapp_owner -d siapp -tAc \
    "SELECT string_agg(format('%I.%I', schemaname, tablename), ', ' ORDER BY tablename)
       FROM pg_tables WHERE schemaname = 'public' AND tablename NOT LIKE 'goose%'")"
  [ -n "$WIPE_TABLES" ] || die "found no tables to wipe, is the schema migrated?"
  PERSONNEL_COUNT="$(dc exec -T postgres psql -U siapp_owner -d siapp -tAc "SELECT count(*) FROM personnels" | tr -d '[:space:]')"
  echo "This will DELETE ALL DATA in database 'siapp' ($COMPOSE_FILE, $PROJECT_DIR):"
  echo "  every table except goose migration history, $PERSONNEL_COUNT personnel row(s) among them."
  echo "  Personalia is not touched."
  if [ "${ASSUME_YES:-0}" = "1" ]; then
    echo "ASSUME_YES=1: not asking."
  else
    [ -r /dev/tty ] || die "no terminal to ask on. Run it from an interactive shell, or pass ASSUME_YES=1 (or SKIP_WIPE=1)."
    printf "Type the database name (siapp) to confirm, anything else aborts: " > /dev/tty
    read -r CONFIRM < /dev/tty || CONFIRM=""
    [ "$CONFIRM" = "siapp" ] || die "not confirmed, nothing was deleted"
  fi
  dc exec -T postgres psql -U siapp_owner -d siapp -v ON_ERROR_STOP=1 -q \
    -c "TRUNCATE TABLE $WIPE_TABLES RESTART IDENTITY CASCADE"
  echo "wiped"
fi

PSQL() {
  dc exec -T postgres psql -U siapp_owner -d siapp -v ON_ERROR_STOP=1 -q \
    -v org="$ORG_ID" -v company="$COMPANY_ID" -v every_day="$EVERY_DAY_SHIFT" \
    -v attendance="$([ "$SKIP_ATTENDANCE" = "1" ] && echo false || echo true)" \
    -v danru_uid="$DANRU_UID" -v ang1_uid="$ANG1_UID" -v ang2_uid="$ANG2_UID" "$@"
}

# ---------- 1. hierarchy + people ----------
say "1/8 Organization, hierarchy, people, memberships"
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

say "2/8 Starting shifts and shift group (seed shifts)"
SEED shifts

say "3/8 Default field permissions (seed permissions)"
SEED permissions

# ---------- 4. first admin ----------
say "4/8 First ORG_ADMIN ($ADMIN_NIK)"
if SEED grant-admin --nik "$ADMIN_NIK"; then
  echo "ok"
else
  warn "grant-admin failed. It asks Personalia for the NIK, so SiAPP must reach Personalia first"
  warn "(PERSONEL_BASE_URL / key in .env) and $ADMIN_NIK must exist there. Fix that and re-run (safe)."
fi

# ---------- 5. schedules, leave, attendance ----------
say "5/8 Schedules, leave data and attendance"
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
say "6/8 Patrol zones, checkpoints and checklists"
PSQL <<'SQL'
SELECT set_config('app.current_org', :'org', false) \gset
BEGIN;

-- Three zones in project 1K-DEMO-S, all near its GPS point (a 50 m radius; the second is about
-- 60 m north of the first, so a walked patrol has a line to draw). A zone is told apart by its
-- name inside project + type, so running this again keeps what is there.
INSERT INTO patrol_zones (project_code, team_code, type, name, alias, description, popup_text,
                          latitude, longitude, radius_meters, min_visit, photo_required, qr_secret)
SELECT '1K-DEMO-S', v.team_code, v.type::patrol_zone_type, v.name, v.alias, v.description, v.popup_text,
       v.latitude, v.longitude, 50, v.min_visit, v.photo_required,
       -- 16 random bytes as base64url, 22 characters, like the app makes them.
       substr(translate(encode(decode(md5(random()::text || v.name), 'hex'), 'base64'), '+/=', '-_'), 1, 22)
FROM (VALUES
  ('Pos Gerbang Utama', 'PATROL', NULL::text,   'Gerbang',  'Gerbang depan proyek demo', 'Periksa pagar dan lampu gerbang.',
   -6.175392::numeric, 106.827153::numeric, 1, false),
  ('Pos Belakang',      'PATROL', NULL::text,   'Belakang', 'Pintu belakang gedung',     NULL::text,
   -6.174850::numeric, 106.827153::numeric, 1, false),
  ('Pos Jaga Depan',    'GUARD',  '1K-DEMO-S1', 'Jaga',     'Pos jaga regu 1',           NULL::text,
   -6.175392::numeric, 106.827600::numeric, 1, true)
) AS v(name, type, team_code, alias, description, popup_text, latitude, longitude, min_visit, photo_required)
ON CONFLICT DO NOTHING;

-- The questions, only for a zone that has none yet, so an admin's edits are never overwritten.
INSERT INTO patrol_checklist_items (zone_id, sort_order, label, help_text, answer_type, options,
                                    required, photo_mode, note_mode, attach_when)
SELECT z.id, c.sort_order, c.label, c.help_text, c.answer_type::checklist_answer_type, c.options,
       c.required, c.photo_mode::checklist_attach_mode, c.note_mode::checklist_attach_mode,
       c.attach_when::checklist_attach_when
FROM (VALUES
  ('Pos Gerbang Utama', 'PATROL', 1, 'Pagar terkunci dan utuh?', 'Foto wajib bila jawabannya tidak.',
   'BOOLEAN', NULL::jsonb, true, 'REQUIRED', 'NONE', 'ON_NO'),
  ('Pos Gerbang Utama', 'PATROL', 2, 'Kondisi lampu', NULL,
   'CHOICE', '[{"value":"baik","label":"Baik"},{"value":"redup","label":"Redup"},{"value":"mati","label":"Mati"}]'::jsonb,
   true, 'NONE', 'NONE', 'ALWAYS'),
  ('Pos Gerbang Utama', 'PATROL', 3, 'Catatan lain', NULL,
   'TEXT', NULL::jsonb, false, 'NONE', 'OPTIONAL', 'ALWAYS'),
  ('Pos Belakang', 'PATROL', 1, 'Pintu belakang terkunci?', NULL,
   'BOOLEAN', NULL::jsonb, true, 'NONE', 'REQUIRED', 'ON_NO'),
  ('Pos Jaga Depan', 'GUARD', 1, 'Jumlah tamu hari ini', NULL,
   'NUMBER', NULL::jsonb, false, 'NONE', 'NONE', 'ALWAYS')
) AS c(zone_name, zone_type, sort_order, label, help_text, answer_type, options, required, photo_mode, note_mode, attach_when)
JOIN patrol_zones z ON z.project_code = '1K-DEMO-S' AND z.name = c.zone_name
                   AND z.type = c.zone_type::patrol_zone_type AND z.deleted_at IS NULL
WHERE NOT EXISTS (SELECT 1 FROM patrol_checklist_items i WHERE i.zone_id = z.id);

-- Checkpoints: a few dozen metres apart around the project point, each with a 30 m radius and its own QR
-- secret (made like the zones'). A checkpoint is told apart by its name inside the project, so running this
-- again keeps what is there.
INSERT INTO patrol_checkpoints (project_code, name, description, latitude, longitude, radius_meters, qr_secret)
SELECT '1K-DEMO-S', v.name, v.description, v.latitude, v.longitude, 30,
       substr(translate(encode(decode(md5(random()::text || v.name), 'hex'), 'base64'), '+/=', '-_'), 1, 22)
FROM (VALUES
  ('Gerbang Utama',       'Gerbang depan proyek demo',   -6.175392::numeric, 106.827153::numeric),
  ('Pagar Timur',         'Pagar sisi timur',            -6.175392::numeric, 106.827400::numeric),
  ('Pintu Belakang',      'Pintu belakang gedung',       -6.174850::numeric, 106.827153::numeric),
  ('Ruang Panel Listrik', 'Panel listrik utama',         -6.174850::numeric, 106.827400::numeric),
  ('Pos Jaga',            'Meja jaga depan',             -6.175392::numeric, 106.827600::numeric)
) AS v(name, description, latitude, longitude)
ON CONFLICT DO NOTHING;

-- The order of each zone, only for a zone that has none yet. Pintu Belakang is in two PATROL zones and Gerbang
-- Utama in a PATROL and a GUARD zone, each at its own position there.
INSERT INTO patrol_zone_checkpoints (project_code, zone_id, checkpoint_id, sort_order)
SELECT '1K-DEMO-S', z.id, c.id, l.position
FROM (VALUES
  ('Pos Gerbang Utama', 'PATROL', 'Gerbang Utama',       1),
  ('Pos Gerbang Utama', 'PATROL', 'Pagar Timur',         2),
  ('Pos Gerbang Utama', 'PATROL', 'Pintu Belakang',      3),
  ('Pos Belakang',      'PATROL', 'Pintu Belakang',      1),
  ('Pos Belakang',      'PATROL', 'Ruang Panel Listrik', 2),
  ('Pos Jaga Depan',    'GUARD',  'Pos Jaga',            1),
  ('Pos Jaga Depan',    'GUARD',  'Gerbang Utama',       2)
) AS l(zone_name, zone_type, checkpoint_name, position)
JOIN patrol_zones z ON z.project_code = '1K-DEMO-S' AND z.name = l.zone_name
                   AND z.type = l.zone_type::patrol_zone_type AND z.deleted_at IS NULL
JOIN patrol_checkpoints c ON c.project_code = '1K-DEMO-S' AND lower(c.name) = lower(l.checkpoint_name) AND c.deleted_at IS NULL
WHERE NOT EXISTS (SELECT 1 FROM patrol_zone_checkpoints x WHERE x.zone_id = z.id);

-- Each checkpoint's checklist, only for a checkpoint that has none (even a deactivated action counts as
-- one), so an admin's edits are never overwritten. It covers every rule: a photo required (min 1), several
-- photos, an optional photo (min 0), no photo at all (max 0), and a note required.
INSERT INTO patrol_checkpoint_actions (checkpoint_id, sort_order, action, instruction, min_photos, max_photos, is_note_required)
SELECT c.id, a.sort_order, a.action, a.instruction, a.min_photos, a.max_photos, a.is_note_required
FROM (VALUES
  ('Gerbang Utama',       0, 'Periksa gembok gerbang', 'Tarik gembok untuk memastikan terkunci.',     1, 2, false),
  ('Gerbang Utama',       1, 'Periksa lampu gerbang',  NULL::text,                                     0, 1, true),
  ('Pagar Timur',         0, 'Periksa pagar',          'Pastikan pagar utuh dan tidak ada yang rusak.', 1, 3, false),
  ('Pintu Belakang',      0, 'Pastikan pintu terkunci', NULL::text,                                    1, 1, false),
  ('Ruang Panel Listrik', 0, 'Cek panel',              NULL::text,                                     1, 2, true),
  ('Ruang Panel Listrik', 1, 'Cek APAR',               'Pastikan segel dan tekanan APAR normal.',      0, 0, true),
  ('Pos Jaga',            0, 'Isi buku tamu',          NULL::text,                                     1, 1, false)
) AS a(checkpoint_name, sort_order, action, instruction, min_photos, max_photos, is_note_required)
JOIN patrol_checkpoints c ON c.project_code = '1K-DEMO-S' AND lower(c.name) = lower(a.checkpoint_name) AND c.deleted_at IS NULL
WHERE NOT EXISTS (SELECT 1 FROM patrol_checkpoint_actions x WHERE x.checkpoint_id = c.id);

COMMIT;

\pset footer off
\echo Patrol zones
SELECT z.id, z.type, z.name, z.team_code, z.radius_meters, count(i.id) AS questions
FROM patrol_zones z LEFT JOIN patrol_checklist_items i ON i.zone_id = z.id
WHERE z.project_code = '1K-DEMO-S' AND z.deleted_at IS NULL
GROUP BY z.id ORDER BY z.id;
\echo Checkpoints of each zone, in order (qr_payload is the text to turn into a QR code and scan)
SELECT z.name AS zone, zc.sort_order AS pos, c.name AS checkpoint,
       (SELECT count(*) FROM patrol_checkpoint_actions a WHERE a.checkpoint_id = c.id AND a.active) AS actions,
       'PERSIST-CP:1:' || :'org' || ':' || c.id || ':' || c.qr_secret AS qr_payload
FROM patrol_zone_checkpoints zc
JOIN patrol_zones z ON z.id = zc.zone_id
JOIN patrol_checkpoints c ON c.id = zc.checkpoint_id
WHERE z.project_code = '1K-DEMO-S' AND z.deleted_at IS NULL
ORDER BY z.name, zc.sort_order;
SQL

# ---------- 7. patrol: routes, scans, approvals, zone check-ins ----------
say "7/8 Patrol routes, scans (pending / approved / declined) and zone check-ins"
PSQL <<'SQL'
SELECT set_config('app.current_org', :'org', false) \gset
BEGIN;

-- Patrol routes. anggota1 walked one on each of the last two scheduled days (ENDED, with a path);
-- anggota2 has one running now (ACTIVE, no path yet). The dates are project dates (Asia/Jakarta).
WITH me AS (SELECT (now() AT TIME ZONE 'Asia/Jakarta')::date AS today)
INSERT INTO patrol_routes (local_id, personnel_id, team_code, project_code, status, started_at, ended_at, work_date,
                           path, distance_meters, point_count, scan_count, last_point_at)
SELECT md5('seed-route-' || p.id || '-' || sc.work_date)::uuid, p.id, '1K-DEMO-S1', '1K-DEMO-S', 'ENDED'::patrol_route_status,
       t.s, t.s + interval '40 minutes', sc.work_date,
       jsonb_build_array(
         jsonb_build_object('lat', -6.175392, 'lng', 106.827153, 'at', t.s,                          'acc', 8),
         jsonb_build_object('lat', -6.175122, 'lng', 106.827153, 'at', t.s + interval '18 minutes', 'acc', 8),
         jsonb_build_object('lat', -6.174852, 'lng', 106.827153, 'at', t.s + interval '36 minutes', 'acc', 8)),
       60, 3, 0, t.s + interval '40 minutes'
FROM me
CROSS JOIN personnels p
JOIN schedules sc ON sc.personnel_id = p.id AND sc.work_date BETWEEN me.today - 2 AND me.today - 1
JOIN shifts sh ON sh.id = sc.shift_id AND NOT sh.is_off
CROSS JOIN LATERAL (SELECT ((sc.work_date::timestamp + interval '9 hours 55 minutes') AT TIME ZONE 'Asia/Jakarta') AS s) t
WHERE p.nik = 'KPIANG0001'
ON CONFLICT DO NOTHING;

INSERT INTO patrol_routes (local_id, personnel_id, team_code, project_code, status, started_at, work_date,
                           distance_meters, point_count, scan_count, last_point_at)
SELECT md5('seed-route-active-' || p.id)::uuid, p.id, '1K-DEMO-S1', '1K-DEMO-S', 'ACTIVE'::patrol_route_status,
       now() - interval '40 minutes', (now() AT TIME ZONE 'Asia/Jakarta')::date, 0, 3, 0, now() - interval '4 minutes'
FROM personnels p WHERE p.nik = 'KPIANG0002'
ON CONFLICT DO NOTHING;

-- The raw points of every route: three, 18 minutes apart, walking north.
INSERT INTO patrol_route_points (patrol_id, seq, logged_at, latitude, longitude, accuracy_meters)
SELECT r.id, g, r.started_at + g * interval '18 minutes', -6.175392 + g * 0.00027, 106.827153, 8
FROM patrol_routes r CROSS JOIN generate_series(0, 2) g
ON CONFLICT DO NOTHING;

-- Scans of the last 3 days (today included) for each scheduled day of the two members, one per
-- zone, at the zone's own point. anggota1 also scans the guard post. Outcome by age:
--   today            SUBMITTED (waiting for the danru: shows in /patrol/pending-approvals)
--   yesterday        anggota1 SUBMITTED, anggota2 APPROVED
--   2 days ago       DECLINED (with a note)
--   3 days ago       APPROVED
WITH me AS (SELECT (now() AT TIME ZONE 'Asia/Jakarta')::date AS today),
danru AS (SELECT id FROM personnels WHERE nik = 'KPIDANRU01'),
src AS (
  SELECT p.id AS pid, p.nik, sc.work_date, (me.today - sc.work_date) AS ago,
         z.id AS zone_id, z.type AS zone_type, z.name AS zone_name,
         z.latitude AS zone_lat, z.longitude AS zone_lng
  FROM me
  CROSS JOIN team_members tm
  JOIN personnels p ON p.id = tm.personnel_id AND p.nik IN ('KPIANG0001', 'KPIANG0002')
  JOIN schedules sc ON sc.personnel_id = p.id AND sc.work_date BETWEEN me.today - 3 AND me.today
                   AND tm.start_date <= sc.work_date
  JOIN shifts sh ON sh.id = sc.shift_id AND NOT sh.is_off
  JOIN patrol_zones z ON z.project_code = '1K-DEMO-S' AND z.deleted_at IS NULL
                     AND (z.type = 'PATROL' OR p.nik = 'KPIANG0001')
  WHERE tm.team_code = '1K-DEMO-S1' AND tm.end_date IS NULL
), timed AS (
  SELECT src.*,
         LEAST(((work_date::timestamp + interval '10 hours' + (zone_id % 3) * interval '10 minutes') AT TIME ZONE 'Asia/Jakarta'),
               now() - interval '10 minutes') AS occurred_at,
         CASE WHEN ago = 0 THEN 'SUBMITTED'
              WHEN ago = 1 THEN CASE WHEN nik = 'KPIANG0001' THEN 'SUBMITTED' ELSE 'APPROVED' END
              WHEN ago = 2 THEN 'DECLINED'
              ELSE 'APPROVED' END AS st
  FROM src
)
INSERT INTO patrol_scans (local_id, zone_id, zone_type, personnel_id, team_code, project_code, occurred_at, received_at, work_date,
                          latitude, longitude, accuracy_meters, distance_meters, subject, notes, answers, status,
                          decided_by_id, decided_at, decision_note, patrol_id, submitted_offline)
SELECT md5('seed-scan-' || t.pid || '-' || t.work_date || '-' || t.zone_id)::uuid, t.zone_id, t.zone_type, t.pid,
       '1K-DEMO-S1', '1K-DEMO-S', t.occurred_at, t.occurred_at + interval '1 minute', t.work_date,
       t.zone_lat, t.zone_lng, 8, 12,
       CASE WHEN t.zone_type = 'PATROL' THEN 'Patroli ' ELSE 'Jaga ' END || t.zone_name,
       CASE WHEN t.st = 'DECLINED' THEN 'Foto kurang jelas' END,
       '[]'::jsonb, t.st::patrol_scan_status,
       CASE WHEN t.st <> 'SUBMITTED' THEN (SELECT id FROM danru) END,
       CASE WHEN t.st <> 'SUBMITTED' THEN t.occurred_at + interval '1 hour' END,
       CASE t.st WHEN 'DECLINED' THEN 'Foto tidak jelas, ulangi patroli' WHEN 'APPROVED' THEN 'OK' END,
       (SELECT r.id FROM patrol_routes r WHERE r.personnel_id = t.pid AND r.work_date = t.work_date AND r.status <> 'ACTIVE' LIMIT 1),
       false
FROM timed t
ON CONFLICT DO NOTHING;

UPDATE patrol_routes r SET scan_count = (SELECT count(*) FROM patrol_scans s WHERE s.patrol_id = r.id);

-- Zone check-ins: anggota1 sat at the guard post on past scheduled days (closed); anggota2 is
-- at the front gate right now (open: a guard is at one post at a time).
INSERT INTO zone_checkins (zone_id, personnel_id, team_code, shift_id, check_in_at, check_in_lat, check_in_lng,
                           check_out_at, check_out_lat, check_out_lng)
SELECT z.id, p.id, '1K-DEMO-S1', sc.shift_id, t.ci, z.latitude, z.longitude, t.ci + interval '8 hours',
       z.latitude, z.longitude
FROM (SELECT (now() AT TIME ZONE 'Asia/Jakarta')::date AS today) me
CROSS JOIN personnels p
JOIN schedules sc ON sc.personnel_id = p.id AND sc.work_date BETWEEN me.today - 3 AND me.today - 1
JOIN shifts sh ON sh.id = sc.shift_id AND NOT sh.is_off
JOIN patrol_zones z ON z.project_code = '1K-DEMO-S' AND z.name = 'Pos Jaga Depan' AND z.deleted_at IS NULL
CROSS JOIN LATERAL (SELECT ((sc.work_date::timestamp + interval '8 hours') AT TIME ZONE 'Asia/Jakarta') AS ci) t
WHERE p.nik = 'KPIANG0001'
  AND NOT EXISTS (SELECT 1 FROM zone_checkins x WHERE x.check_out_at IS NOT NULL);

INSERT INTO zone_checkins (zone_id, personnel_id, team_code, shift_id, check_in_at, check_in_lat, check_in_lng)
SELECT z.id, p.id, '1K-DEMO-S1',
       (SELECT sc.shift_id FROM schedules sc JOIN shifts sh ON sh.id = sc.shift_id AND NOT sh.is_off
         WHERE sc.personnel_id = p.id AND sc.work_date = (now() AT TIME ZONE 'Asia/Jakarta')::date),
       now() - interval '30 minutes', z.latitude, z.longitude
FROM personnels p
JOIN patrol_zones z ON z.project_code = '1K-DEMO-S' AND z.name = 'Pos Gerbang Utama' AND z.deleted_at IS NULL
WHERE p.nik = 'KPIANG0002'
  AND NOT EXISTS (SELECT 1 FROM zone_checkins x WHERE x.personnel_id = p.id AND x.check_out_at IS NULL);

COMMIT;
SQL
echo "ok"

# ---------- 8. leave requests, shift swap, location, notifications, master data ----------
say "8/8 Leave requests, shift swap, location, notifications and master data"
PSQL <<'SQL'
SELECT set_config('app.current_org', :'org', false) \gset
BEGIN;

-- Leave requests: pending (the danru has to decide), approved (with a ledger deduction), rejected,
-- cancelled. Numbers are LR / PR / SR + YYYYMM + a 5-digit counter per type, as the app makes them.
INSERT INTO leave_requests (number, personnel_id, team_code, project_code, type, category_id, start_date, end_date,
                            working_days, reason, status, approver_personnel_id, decided_level, decided_by_id,
                            decided_at, decision_note, cancelled_at)
SELECT v.pfx || to_char(me.today, 'YYYYMM') || lpad(v.seq::text, 5, '0'), p.id, '1K-DEMO-S1', '1K-DEMO-S', c.type, c.id,
       me.today + v.s, me.today + v.e, v.wd, v.reason, v.status::leave_request_status, d.id,
       CASE WHEN v.status IN ('APPROVED', 'REJECTED') THEN 'DANRU'::leave_decided_level END,
       CASE WHEN v.status IN ('APPROVED', 'REJECTED') THEN d.id END,
       CASE WHEN v.status IN ('APPROVED', 'REJECTED') THEN now() - interval '1 day' END,
       v.note,
       CASE WHEN v.status = 'CANCELLED' THEN now() - interval '8 days' END
FROM (SELECT (now() AT TIME ZONE 'Asia/Jakarta')::date AS today) me
CROSS JOIN (VALUES
  ('LR', 1, 'KPIANG0001', 'ANNUAL',   3,   4, 2, 'Acara keluarga',              'PENDING',   NULL::text),
  ('PR', 1, 'KPIANG0002', 'PERMIT',   1,   1, 1, 'Mengurus dokumen',            'PENDING',   NULL::text),
  ('LR', 2, 'KPIANG0002', 'ANNUAL', -14, -13, 2, 'Liburan keluarga',            'APPROVED',  'Disetujui'),
  ('PR', 2, 'KPIANG0001', 'PERMIT', -10, -10, 1, 'Keperluan pribadi',           'REJECTED',  'Kekuatan regu tidak cukup'),
  ('SR', 1, 'KPIANG0001', 'SICK',   -20, -19, 0, 'Demam dan flu',               'APPROVED',  'Semoga lekas sembuh'),
  ('LR', 3, 'KPIANG0002', 'ANNUAL',  -8,  -8, 1, 'Dibatalkan, jadwal berubah',  'CANCELLED', NULL::text)
) AS v(pfx, seq, nik, cat, s, e, wd, reason, status, note)
JOIN personnels p ON p.nik = v.nik
JOIN leave_categories c ON c.code = v.cat
CROSS JOIN (SELECT id FROM personnels WHERE nik = 'KPIDANRU01') d
ON CONFLICT DO NOTHING;

-- An approved request of a category with a quota is deducted from the ledger once (12 days granted).
INSERT INTO leave_ledger (personnel_id, kind, days, balance_after, leave_request_id, note, created_by_id)
SELECT lr.personnel_id, 'DEDUCT'::leave_ledger_kind, -lr.working_days, 12 - lr.working_days, lr.id, 'dev seed', lr.decided_by_id
FROM leave_requests lr
JOIN leave_categories c ON c.id = lr.category_id AND c.has_quota AND c.is_annual
WHERE lr.status = 'APPROVED' AND lr.working_days > 0
ON CONFLICT DO NOTHING;

INSERT INTO leave_request_counters (type, counter)
SELECT type, count(*) FROM leave_requests GROUP BY type
ON CONFLICT (organization_id, type) DO UPDATE SET counter = EXCLUDED.counter;

-- Shift swap: the danru asked anggota1 to cover the newest absence (mangkir) of anyone else.
-- Nothing is made when there is no absence in the last 6 days.
INSERT INTO shift_swaps (replaced_personnel_id, replaced_attendance_id, replacing_personnel_id, team_code, work_date,
                         shift_id, starts_at, ends_at, note, created_by_id)
SELECT a.personnel_id, a.id, r.id, a.team_code, a.work_date, a.shift_id, a.shift_starts_at, a.shift_ends_at,
       'Tukar jaga (dev seed)', d.id
FROM attendances a
CROSS JOIN (SELECT id FROM personnels WHERE nik = 'KPIANG0001') r
CROSS JOIN (SELECT id FROM personnels WHERE nik = 'KPIDANRU01') d
WHERE a.status = 'ABSENT' AND a.personnel_id <> r.id
ORDER BY a.work_date DESC
LIMIT 1
ON CONFLICT DO NOTHING;

-- Location: a point every 15 minutes for the last 3 hours of each person, and the newest of each.
INSERT INTO location_logs (personnel_id, logged_at, latitude, longitude, accuracy_meters, battery_pct)
SELECT p.id, date_trunc('minute', now()) - g * interval '15 minutes',
       -6.175392 + ((12 - g) * 0.00002) + (p.id % 3) * 0.00005, 106.827153 + ((12 - g) * 0.00001),
       8 + (g % 5), 90 - g
FROM personnels p CROSS JOIN generate_series(0, 11) g
WHERE p.nik IN ('KPIDANRU01', 'KPIANG0001', 'KPIANG0002')
  AND NOT EXISTS (SELECT 1 FROM location_logs)
ON CONFLICT DO NOTHING;

INSERT INTO personnel_last_locations (personnel_id, logged_at, latitude, longitude, accuracy_meters)
SELECT DISTINCT ON (personnel_id) personnel_id, logged_at, latitude, longitude, accuracy_meters
FROM location_logs ORDER BY personnel_id, logged_at DESC
ON CONFLICT (organization_id, personnel_id) DO UPDATE
  SET logged_at = EXCLUDED.logged_at, latitude = EXCLUDED.latitude, longitude = EXCLUDED.longitude,
      accuracy_meters = EXCLUDED.accuracy_meters
  WHERE EXCLUDED.logged_at > personnel_last_locations.logged_at;

-- Master data: the same defaults the app gives an organization at its first login, so that login
-- adds nothing, plus a default letterhead.
INSERT INTO stakeholder_types (code, name, default_phone, sort_order) VALUES
  ('POLICE', 'Polisi', '110', 0), ('HOSPITAL', 'Rumah Sakit', '119', 1), ('FIRE_DEPARTMENT', 'Pemadam Kebakaran', '113', 2)
ON CONFLICT DO NOTHING;

INSERT INTO panic_types (code, name, stakeholder_type_id, sort_order)
SELECT v.code, v.name, (SELECT id FROM stakeholder_types WHERE code = v.stakeholder), v.sort_order
FROM (VALUES ('SECURITY', 'Keamanan', 'POLICE', 0), ('MEDICAL', 'Medis', 'HOSPITAL', 1),
             ('FIRE', 'Kebakaran', 'FIRE_DEPARTMENT', 2), ('OTHER', 'Lainnya', NULL::text, 3)) AS v(code, name, stakeholder, sort_order)
ON CONFLICT DO NOTHING;

INSERT INTO categories (kind, name, urgency, sort_order)
SELECT v.kind::category_kind, v.name, v.urgency::urgency, v.sort_order
FROM (VALUES
  ('REPORT', 'Penemuan Barang', NULL::text, 0), ('REPORT', 'Kejadian', NULL::text, 1),
  ('REPORT', 'Kehilangan', NULL::text, 2),      ('REPORT', 'Lainnya', NULL::text, 3),
  ('INSTRUCTION', 'Umum', 'NORMAL', 0),         ('INSTRUCTION', 'Pengecekan', 'HIGH', 1)
) AS v(kind, name, urgency, sort_order)
WHERE NOT EXISTS (SELECT 1 FROM categories c WHERE c.kind = v.kind::category_kind)
ON CONFLICT DO NOTHING;

INSERT INTO app_links (name, icon_key, sort_order, active) VALUES
  ('E-Koperasi Janusa', 'koperasi', 0, false), ('IKI', 'iki', 1, false)
ON CONFLICT DO NOTHING;

INSERT INTO letterheads (name, header_text, footer_text, is_default)
VALUES ('KPI Group', 'PT KPI Group', 'Dokumen internal, dibuat otomatis oleh SiAPP', true)
ON CONFLICT DO NOTHING;

-- The inbox: what the danru has waiting (leave and patrol to decide), what the members were told.
INSERT INTO notifications (recipient_personnel_id, type, title, body, entity_type, entity_id, data)
SELECT d.id, 'LEAVE_REQUESTED'::notification_type, 'Pengajuan baru: ' || c.name,
       p.full_name || ' mengajukan ' || lower(c.name) || ' ' || to_char(lr.start_date, 'DD-MM-YYYY'),
       'leave_request', lr.id, jsonb_build_object('leave_request_id', lr.id, 'team_code', lr.team_code)
FROM leave_requests lr
JOIN personnels p ON p.id = lr.personnel_id
JOIN leave_categories c ON c.id = lr.category_id
CROSS JOIN (SELECT id FROM personnels WHERE nik = 'KPIDANRU01') d
WHERE lr.status = 'PENDING'
  AND NOT EXISTS (SELECT 1 FROM notifications n WHERE n.type = 'LEAVE_REQUESTED' AND n.entity_id = lr.id);

INSERT INTO notifications (recipient_personnel_id, type, title, body, entity_type, entity_id, data)
SELECT lr.personnel_id, 'LEAVE_DECIDED'::notification_type,
       CASE lr.status WHEN 'APPROVED' THEN 'Pengajuan disetujui' ELSE 'Pengajuan ditolak' END,
       lr.number || ': ' || coalesce(lr.decision_note, '-'),
       'leave_request', lr.id, jsonb_build_object('leave_request_id', lr.id, 'status', lr.status)
FROM leave_requests lr
WHERE lr.status IN ('APPROVED', 'REJECTED')
  AND NOT EXISTS (SELECT 1 FROM notifications n WHERE n.type = 'LEAVE_DECIDED' AND n.entity_id = lr.id);

INSERT INTO notifications (recipient_personnel_id, type, title, body, entity_type, entity_id, data)
SELECT d.id, (CASE WHEN s.zone_type = 'PATROL' THEN 'PATROL_SUBMITTED' ELSE 'GUARD_SUBMITTED' END)::notification_type,
       'Menunggu persetujuan', p.full_name || ': ' || s.subject,
       'patrol_scan', s.id, jsonb_build_object('scan_id', s.id, 'team_code', s.team_code)
FROM patrol_scans s
JOIN personnels p ON p.id = s.personnel_id
CROSS JOIN (SELECT id FROM personnels WHERE nik = 'KPIDANRU01') d
WHERE s.status = 'SUBMITTED'
  AND NOT EXISTS (SELECT 1 FROM notifications n WHERE n.entity_type = 'patrol_scan' AND n.entity_id = s.id AND n.recipient_personnel_id = d.id);

INSERT INTO notifications (recipient_personnel_id, type, title, body, entity_type, entity_id, data, read_at)
SELECT s.personnel_id, (CASE WHEN s.zone_type = 'PATROL' THEN 'PATROL_APPROVED' ELSE 'GUARD_APPROVED' END)::notification_type,
       'Disetujui', s.subject, 'patrol_scan', s.id, jsonb_build_object('scan_id', s.id),
       CASE WHEN s.id % 2 = 0 THEN now() END
FROM patrol_scans s
WHERE s.status = 'APPROVED'
  AND NOT EXISTS (SELECT 1 FROM notifications n WHERE n.entity_type = 'patrol_scan' AND n.entity_id = s.id AND n.recipient_personnel_id = s.personnel_id);

COMMIT;
SQL
echo "ok"

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
\echo Patrol scans by status
SELECT status, count(*) AS scans FROM patrol_scans GROUP BY status ORDER BY status;
\echo Leave requests by status
SELECT status, count(*) AS requests FROM leave_requests GROUP BY status ORDER BY status;
\echo Rows per seeded table
SELECT 'patrol_routes' AS tbl, count(*) FROM patrol_routes
UNION ALL SELECT 'zone_checkins', count(*) FROM zone_checkins
UNION ALL SELECT 'shift_swaps', count(*) FROM shift_swaps
UNION ALL SELECT 'location_logs', count(*) FROM location_logs
UNION ALL SELECT 'notifications', count(*) FROM notifications
UNION ALL SELECT 'panic_types', count(*) FROM panic_types
UNION ALL SELECT 'categories', count(*) FROM categories;
SQL

cat <<EOF

Test login body (POST /api/v1/auth/login). The password is whatever the account has in Personalia:
  { "identifier": "danru.kpi@siapp.test", "password": "<password>",
    "client": "MOBILE", "device_token": "my-test-phone-001", "remember_me": true }

People: danru KPIDANRU01, members KPIANG0001 and KPIANG0002, admin $ADMIN_NIK (ORG_ADMIN, if step 4 worked)
Regu 1K-DEMO-S1 at project 1K-DEMO-S, GPS -6.175392, 106.827153 (radius 150 m).
Today has no attendance rows: check in as a member to test it.
Patrol zones: Pos Gerbang Utama, Pos Belakang (PATROL) and Pos Jaga Depan (GUARD), see the table above.
Each zone has checkpoints in order (see step 6): a guard scans them one after the other, and each has its own
checklist. The "qr_payload" column of step 6 is what the checkpoint's QR code holds: make a QR code from it
(or print one from the admin panel: GET /api/v1/admin/checkpoints/{id}/qr).
EOF
