package patrol

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"siapp/internal/db"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/timeutil"
)

// withFieldBundle fills the zones of the field list with what GET /zones/{id} shows for each (Phase 10.18,
// BE-CP2 and BE-CP3 of the mobile feedback): active checkpoints with their checklists, my_scans, scannable
// and next_checkpoint_id. The app reads the lot once, keeps it for offline scans, and finds the zone of a
// scanned QR from it.
//
// ⚠ It must say exactly what the detail of each zone says. The shaping is the same function (fieldZoneOf) and
// the order rule the same (orderOf); only the reading is batched: one query for the checkpoints of all the
// zones, one for the counts, one for the checklists of every distinct checkpoint (a checkpoint in two zones
// is read once), however many zones there are. The work date is still the zone's own (see workDates), so a
// night-shift guard reads the same walk here as in the detail. A test holds the two equal.
func (s *Service) withFieldBundle(ctx context.Context, q *db.Queries, actor auth.Principal, rows []db.PatrolZone, zones []Zone) ([]Zone, error) {
	if len(rows) == 0 {
		return zones, nil
	}
	zoneIDs := make([]int64, 0, len(rows))
	for _, r := range rows {
		zoneIDs = append(zoneIDs, r.ID)
	}
	linkRows, err := q.ListCheckpointsOfZones(ctx, zoneIDs)
	if err != nil {
		return nil, fmt.Errorf("list checkpoints of zones: %w", err)
	}
	linksOf := make(map[int64][]linkedCheckpoint, len(rows))
	seen := map[pgtype.UUID]bool{}
	var checkpointIDs []pgtype.UUID
	for _, r := range linkRows {
		linksOf[r.ZoneID] = append(linksOf[r.ZoneID], linkedCheckpoint{cp: bundleLinkRow(r), position: r.Position})
		if !seen[r.ID] {
			seen[r.ID] = true
			checkpointIDs = append(checkpointIDs, r.ID)
		}
	}

	// Only a zone with checkpoints has a walk to read.
	var withLinks []db.PatrolZone
	for _, r := range rows {
		if len(linksOf[r.ID]) > 0 {
			withLinks = append(withLinks, r)
		}
	}
	stepsOf := map[int64][]orderStep{}
	var intervals map[int64]time.Duration
	actionsOf := map[string][]CheckpointAction{}
	if len(withLinks) > 0 {
		dates, ivs, err := s.workDates(ctx, q, actor, withLinks)
		intervals = ivs
		if err != nil {
			return nil, err
		}
		ids := make([]int64, 0, len(withLinks))
		days := make([]pgtype.Date, 0, len(withLinks))
		for _, r := range withLinks {
			ids = append(ids, r.ID)
			days = append(days, pgtype.Date{Time: dates[r.ID].Time(), Valid: true})
		}
		counts, err := q.CountScansByPositionOfZones(ctx, db.CountScansByPositionOfZonesParams{
			ZoneIds: ids, WorkDates: days, PersonnelID: actor.PersonnelID,
		})
		if err != nil {
			return nil, fmt.Errorf("count scans by position of zones: %w", err)
		}
		for _, c := range counts {
			stepsOf[c.ZoneID] = append(stepsOf[c.ZoneID], orderStep{CheckpointID: c.CheckpointID, Position: int(c.Position), Scans: int(c.Scans), LastScannedAt: lastScan(c.LastScannedAt)})
		}
		if actionsOf, err = actionsByCheckpoint(ctx, q, checkpointIDs); err != nil {
			return nil, err
		}
	}

	for i := range zones {
		id := zones[i].ID
		if len(linksOf[id]) == 0 {
			zones[i].Checkpoints = []ZoneCheckpoint{}
			continue
		}
		zones[i] = fieldZoneOf(zones[i], linksOf[id], orderOf(stepsOf[id]), actionsOf, intervals[id])
	}
	return zones, nil
}

// workDates is the work date each zone's next scan by the caller would get: the same placeIn the detail uses,
// asked once for each project and regu the zones come from rather than once for each zone. It also gives each
// zone's patrol interval (scanInterval of its project, which it has read anyway) for next_allowed_at.
func (s *Service) workDates(ctx context.Context, q *db.Queries, actor auth.Principal, rows []db.PatrolZone) (map[int64]timeutil.Date, map[int64]time.Duration, error) {
	type placeKey struct {
		project string
		team    pgtype.Text
	}
	type projectInfo struct {
		loc      *time.Location
		interval time.Duration
	}
	projects := map[string]projectInfo{}
	placed := map[placeKey]timeutil.Date{}
	out := make(map[int64]timeutil.Date, len(rows))
	intervals := make(map[int64]time.Duration, len(rows))
	for _, r := range rows {
		key := placeKey{project: r.ProjectCode, team: r.TeamCode}
		info, ok := projects[r.ProjectCode]
		if !ok {
			project, err := q.GetProject(ctx, r.ProjectCode)
			if err != nil {
				return nil, nil, fmt.Errorf("get project: %w", err)
			}
			loc, err := time.LoadLocation(project.Timezone)
			if err != nil {
				return nil, nil, fmt.Errorf("project %s: timezone %q: %w", project.Code, project.Timezone, err)
			}
			info = projectInfo{loc: loc, interval: scanInterval(project)}
			projects[r.ProjectCode] = info
		}
		day, ok := placed[key]
		if !ok {
			where, err := s.placeIn(ctx, q, actor, r.ProjectCode, r.TeamCode, info.loc, s.clock.Now())
			if err != nil {
				return nil, nil, err
			}
			day = where.workDate
			placed[key] = day
		}
		out[r.ID] = day
		intervals[r.ID] = info.interval
	}
	return out, intervals, nil
}

// bundleLinkRow is the checkpoint columns of a row of ListCheckpointsOfZones.
func bundleLinkRow(r db.ListCheckpointsOfZonesRow) db.PatrolCheckpoint {
	return db.PatrolCheckpoint{
		ID: r.ID, OrganizationID: r.OrganizationID, ProjectCode: r.ProjectCode, Name: r.Name, Description: r.Description,
		Latitude: r.Latitude, Longitude: r.Longitude, RadiusMeters: r.RadiusMeters, QrSecret: r.QrSecret,
		QrRotatedAt: r.QrRotatedAt, Active: r.Active, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, DeletedAt: r.DeletedAt,
	}
}
