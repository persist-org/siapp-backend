-- Scans and zone check-ins (Phase 10.2). Everything runs inside database.WithTenant.

-- Every item of a zone, the deactivated ones too: an offline scan is judged against the checklist
-- as it was when the guard scanned.
-- name: ListAllChecklistItems :many
SELECT * FROM patrol_checklist_items WHERE zone_id = $1 ORDER BY sort_order, id;

-- name: GetPatrolScanByLocalID :one
SELECT * FROM patrol_scans WHERE local_id = $1;

-- name: InsertPatrolScan :one
INSERT INTO patrol_scans (local_id, zone_id, zone_type, personnel_id, team_code, project_code, occurred_at,
                          received_at, work_date, latitude, longitude, accuracy_meters, distance_meters,
                          subject, notes, answers, patrol_id, submitted_offline, created_at, updated_at)
VALUES (@local_id, @zone_id, @zone_type, @personnel_id, @team_code, @project_code, @occurred_at, @received_at,
        @work_date, @latitude, @longitude, @accuracy_meters, sqlc.narg('distance_meters'), @subject,
        sqlc.narg('notes'), @answers, sqlc.narg('patrol_id'), @submitted_offline, @received_at, @received_at)
RETURNING *;

-- A scan of this zone by this person between two instants, a declined one not counting: the
-- patrol interval is about how often a guard visits, and a visit the DANRU turned down did not
-- happen, so the guard may do it again.
-- name: HasScanBetween :one
SELECT EXISTS (
    SELECT 1 FROM patrol_scans
    WHERE personnel_id = @personnel_id AND zone_id = @zone_id AND status <> 'DECLINED'
      AND occurred_at > @after AND occurred_at < @before);

-- A transaction-scoped lock on a name, so two requests about the same thing take turns.
-- name: LockPatrolKey :exec
SELECT pg_advisory_xact_lock(hashtextextended(@key::text, 0));

-- name: InsertZoneCheckin :one
INSERT INTO zone_checkins (zone_id, personnel_id, team_code, shift_id, check_in_at, check_in_lat, check_in_lng,
                           created_at, updated_at)
VALUES (@zone_id, @personnel_id, @team_code, sqlc.narg('shift_id'), @at, @latitude, @longitude, @at, @at)
RETURNING *;

-- Closes the person's open check-in of this zone. No row: there was none.
-- name: CloseZoneCheckin :one
UPDATE zone_checkins SET check_out_at = @at, check_out_lat = @latitude, check_out_lng = @longitude, updated_at = @at
WHERE zone_id = @zone_id AND personnel_id = @personnel_id AND check_out_at IS NULL
RETURNING *;

-- Everyone still checked in to a zone that goes away: nothing else would let them out, and one
-- open check-in blocks a check-in anywhere.
-- name: CloseZoneCheckinsOfZone :execrows
-- The check-out position is the check-in's own (the last place the guard was seen): the table wants
-- one whenever there is a check-out time.
UPDATE zone_checkins SET check_out_at = GREATEST(@at::timestamptz, check_in_at), updated_at = @at,
    check_out_lat = check_in_lat, check_out_lng = check_in_lng
WHERE zone_id = @zone_id AND check_out_at IS NULL;

-- Who is checked in to the zone now. team_codes and personnel_id narrow it to what the caller
-- may see.
-- name: ListZoneLive :many
SELECT c.*, p.full_name FROM zone_checkins c
JOIN personnels p ON p.id = c.personnel_id
WHERE c.zone_id = @zone_id AND c.check_out_at IS NULL
  AND (sqlc.narg('team_codes')::text[] IS NULL OR c.team_code = ANY(sqlc.narg('team_codes')::text[]))
  AND (sqlc.narg('personnel_id')::bigint IS NULL OR c.personnel_id = sqlc.narg('personnel_id'))
ORDER BY c.check_in_at, c.id;

-- name: ListZoneTracking :many
SELECT c.*, p.full_name FROM zone_checkins c
JOIN personnels p ON p.id = c.personnel_id
WHERE c.zone_id = @zone_id AND c.check_in_at >= @from_at AND c.check_in_at < @to_at
  AND (sqlc.narg('team_codes')::text[] IS NULL OR c.team_code = ANY(sqlc.narg('team_codes')::text[]))
  AND (sqlc.narg('personnel_id')::bigint IS NULL OR c.personnel_id = sqlc.narg('personnel_id'))
ORDER BY c.check_in_at DESC, c.id DESC
LIMIT @page_limit OFFSET @page_offset;

-- name: CountZoneTracking :one
SELECT count(*) FROM zone_checkins c
WHERE c.zone_id = @zone_id AND c.check_in_at >= @from_at AND c.check_in_at < @to_at
  AND (sqlc.narg('team_codes')::text[] IS NULL OR c.team_code = ANY(sqlc.narg('team_codes')::text[]))
  AND (sqlc.narg('personnel_id')::bigint IS NULL OR c.personnel_id = sqlc.narg('personnel_id'));
