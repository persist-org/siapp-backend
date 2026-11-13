package patrol

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/timeutil"
)

// MaxBulkDecision is how many scans one bulk approve may carry.
const MaxBulkDecision = 50

// approvePermission is the permission that decides a scan of the zone type: patrol.approve for a
// patrol zone, guard.approve for a guard zone.
func approvePermission(t db.PatrolZoneType) access.Permission {
	if t == db.PatrolZoneTypeGUARD {
		return access.GuardApprove
	}
	return access.PatrolApprove
}

// approveReach returns the regu whose scans of this zone type the person may decide on the date, or
// nil when they may not decide any (a member, or a grant that only reaches their own records).
func (s *Service) approveReach(ctx context.Context, q *db.Queries, personnelID int64, t db.PatrolZoneType, day timeutil.Date) ([]string, error) {
	if s.perms == nil {
		return nil, errors.New("patrol: permissions are not plugged in")
	}
	res, err := s.perms.Resolve(ctx, q, personnelID, approvePermission(t), day)
	if err != nil {
		return nil, err
	}
	if !res.Allowed || res.SelfOnly {
		return nil, nil
	}
	return res.TeamCodes, nil
}

func dateOf(d pgtype.Date) timeutil.Date {
	return timeutil.NewDate(d.Time.Year(), d.Time.Month(), d.Time.Day())
}

// present turns stored scans into what a reader is shown: the author's name, and every photo as a
// signed URL, in the answer it belongs to or among the scan's general photos.
//
// It runs inside the reader's tenant transaction and only after the caller has decided that the
// reader may see these scans: the signed URL is the capability (media.Views asks nobody).
func (s *Service) present(ctx context.Context, q *db.Queries, rows []db.PatrolScan) ([]Scan, error) {
	out := make([]Scan, 0, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]int64, 0, len(rows))
	personnel := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
		personnel = append(personnel, r.PersonnelID)
	}
	names, err := q.ListPersonnelNames(ctx, personnel)
	if err != nil {
		return nil, fmt.Errorf("list names: %w", err)
	}
	nameOf := make(map[int64]string, len(names))
	for _, n := range names {
		nameOf[n.ID] = n.FullName
	}
	photoRows, err := q.ListScanPhotos(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("list scan photos: %w", err)
	}
	attached := make(map[int64][]int64, len(rows))
	for _, p := range photoRows {
		attached[p.ScanID.Int64] = append(attached[p.ScanID.Int64], p.MediaID)
	}

	for _, r := range rows {
		scan, err := scanOf(r)
		if err != nil {
			return nil, err
		}
		scan.PersonnelName = nameOf[r.PersonnelID]
		scan.Photos = []Photo{}

		// The photos of the answers first; what is left of the scan's photos is the general ones.
		inAnswers := map[int64]bool{}
		for i := range scan.Answers {
			a := &scan.Answers[i]
			a.Photos = []Photo{}
			for _, id := range a.MediaIDs {
				inAnswers[id] = true
			}
			photos, err := s.photos(ctx, q, scan.ID, a.MediaIDs)
			if err != nil {
				return nil, err
			}
			a.Photos = photos
		}
		var general []int64
		for _, id := range attached[scan.ID] {
			if !inAnswers[id] {
				general = append(general, id)
			}
		}
		if scan.Photos, err = s.photos(ctx, q, scan.ID, general); err != nil {
			return nil, err
		}
		out = append(out, scan)
	}
	return out, nil
}

func (s *Service) photos(ctx context.Context, q *db.Queries, scanID int64, ids []int64) ([]Photo, error) {
	if len(ids) == 0 {
		return []Photo{}, nil
	}
	if s.attach == nil {
		return nil, errors.New("patrol: photo attachments are not plugged in")
	}
	views, err := s.attach.Views(ctx, q, EntityScan, scanID, ids)
	if err != nil {
		return nil, err
	}
	out := make([]Photo, 0, len(views))
	for _, v := range views {
		out = append(out, Photo{ID: v.ID, URL: v.URL, ContentType: v.ContentType, CapturedAt: v.CapturedAt, CaptureSource: string(v.CaptureSource)})
	}
	return out, nil
}

// ScanFilter narrows a zone's scan list.
type ScanFilter struct {
	PersonnelID int64
	WorkDate    *timeutil.Date
	Status      string
	Limit       int
	Offset      int
}

// ListZoneScans returns one page of a zone's scans, newest first, and their total. A DANRU sees the
// scans of the regu their approve permission reaches; anyone else who serves in the project sees
// their own. Approved and declined scans stay listed (§3.2.6.3).
func (s *Service) ListZoneScans(ctx context.Context, p auth.Principal, zoneID int64, f ScanFilter) ([]Scan, int, error) {
	var out []Scan
	var total int64
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		zone, project, err := loadZone(ctx, q, zoneID)
		if err != nil {
			return err
		}
		sc, err := s.scopeOf(ctx, q, p, zone, project)
		if err != nil {
			return err
		}
		teams := sc.teams
		if teams == nil {
			teams = []string{}
		}
		params := db.ListZoneScansParams{
			ZoneID: zoneID, TeamCodes: teams, ViewerID: p.PersonnelID, PersonnelID: int8Param(positive(f.PersonnelID)),
			WorkDate: dateParam(f.WorkDate), Status: scanStatus(f.Status),
			PageLimit: int32(f.Limit), PageOffset: int32(f.Offset), //nolint:gosec // bounded by ParseListQuery
		}
		rows, err := q.ListZoneScans(ctx, params)
		if err != nil {
			return fmt.Errorf("list zone scans: %w", err)
		}
		total, err = q.CountZoneScans(ctx, db.CountZoneScansParams{
			ZoneID: zoneID, TeamCodes: teams, ViewerID: p.PersonnelID, PersonnelID: params.PersonnelID,
			WorkDate: params.WorkDate, Status: params.Status,
		})
		if err != nil {
			return fmt.Errorf("count zone scans: %w", err)
		}
		out, err = s.present(ctx, q, rows)
		return err
	})
	return out, int(total), err
}

// GetScan returns one scan with its answers and photos. The author reads their own; a DANRU reads
// the scans of the regu they decide for. Any other scan is answered as if it did not exist.
func (s *Service) GetScan(ctx context.Context, p auth.Principal, id int64) (Scan, error) {
	var out Scan
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetPatrolScan(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrScanNotFound
		}
		if err != nil {
			return fmt.Errorf("get scan: %w", err)
		}
		if row.PersonnelID != p.PersonnelID {
			// Judged on the scan's own work date, not today: someone who moved regu this morning
			// still reads yesterday's scans of the regu they decided for then.
			teams, err := s.approveReach(ctx, q, p.PersonnelID, row.ZoneType, dateOf(row.WorkDate))
			if err != nil {
				return err
			}
			if !slices.Contains(teams, row.TeamCode) {
				return ErrScanNotFound
			}
		}
		scans, err := s.present(ctx, q, []db.PatrolScan{row})
		if err != nil {
			return err
		}
		out = scans[0]
		return nil
	})
	return out, err
}

// Pending returns the DANRU's queue: the SUBMITTED scans of the regu they decide for, oldest first,
// patrol and guard zones each by their own permission. Someone who decides for no regu at all is
// PERMISSION_DENIED: the queue is not theirs.
func (s *Service) Pending(ctx context.Context, p auth.Principal, teamCode string, limit, offset int) ([]Scan, int, error) {
	var out []Scan
	var total int64
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		if s.perms == nil {
			return errors.New("patrol: permissions are not plugged in")
		}
		reach := map[db.PatrolZoneType][]string{}
		for _, t := range []db.PatrolZoneType{db.PatrolZoneTypePATROL, db.PatrolZoneTypeGUARD} {
			res, err := s.perms.ResolveToday(ctx, q, p.PersonnelID, approvePermission(t))
			if err != nil {
				return err
			}
			reach[t] = []string{}
			if res.Allowed && !res.SelfOnly {
				reach[t] = res.TeamCodes
			}
		}
		patrolTeams, guardTeams := reach[db.PatrolZoneTypePATROL], reach[db.PatrolZoneTypeGUARD]
		if len(patrolTeams) == 0 && len(guardTeams) == 0 {
			return access.ErrPermissionDenied
		}
		if teamCode != "" && !slices.Contains(patrolTeams, teamCode) && !slices.Contains(guardTeams, teamCode) {
			return access.ErrPermissionDenied
		}
		// ⚠ The queue shows what decide would accept. decide judges each scan's regu on the scan's own
		// work date, so the queue does too: a scan of a regu the caller decided for last week and does
		// not today is not listed, instead of listed and refused with 403 (a whole bulk with it).
		types, teams, days, err := s.decidableKeys(ctx, q, p.PersonnelID, teamCode)
		if err != nil {
			return err
		}
		rows, err := q.ListPendingScans(ctx, db.ListPendingScansParams{
			Types: types, Teams: teams, Days: days, TeamCode: text(teamCode),
			PageLimit: int32(limit), PageOffset: int32(offset), //nolint:gosec // bounded by ParseListQuery
		})
		if err != nil {
			return fmt.Errorf("list pending scans: %w", err)
		}
		total, err = q.CountPendingScans(ctx, db.CountPendingScansParams{Types: types, Teams: teams, Days: days, TeamCode: text(teamCode)})
		if err != nil {
			return fmt.Errorf("count pending scans: %w", err)
		}
		out, err = s.present(ctx, q, rows)
		return err
	})
	return out, int(total), err
}

// decidableKeys returns, as three parallel arrays, the (zone type, regu, work date) combinations of
// the SUBMITTED scans that the person may decide on: each judged by approveReach on its own date,
// the rule decide applies.
func (s *Service) decidableKeys(ctx context.Context, q *db.Queries, personnelID int64, teamCode string) ([]string, []string, []pgtype.Date, error) {
	keys, err := q.ListPendingKeys(ctx, text(teamCode))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list pending keys: %w", err)
	}
	type reachKey struct {
		zoneType db.PatrolZoneType
		day      timeutil.Date
	}
	reach := map[reachKey][]string{}
	types, teams, days := []string{}, []string{}, []pgtype.Date{}
	for _, k := range keys {
		rk := reachKey{k.ZoneType, dateOf(k.WorkDate)}
		list, cached := reach[rk]
		if !cached {
			if list, err = s.approveReach(ctx, q, personnelID, rk.zoneType, rk.day); err != nil {
				return nil, nil, nil, err
			}
			reach[rk] = list
		}
		if slices.Contains(list, k.TeamCode) {
			types, teams, days = append(types, string(k.ZoneType)), append(teams, k.TeamCode), append(days, k.WorkDate)
		}
	}
	return types, teams, days, nil
}

// ZoneRef is a zone as the progress screen lists it.
type ZoneRef struct {
	ID          int64
	ProjectCode string
	TeamCode    *string
	Type        string
	Name        string
	Alias       *string
	MinVisit    int
}

// Progress is the three circles of one zone on one day: the red number of visits it needs
// (Required), the blue ones scanned and not decided yet (Submitted), the green ones approved
// (Approved), and the declined ones.
type Progress struct {
	Zone      ZoneRef
	Required  int
	Submitted int
	Approved  int
	Declined  int
}

// ProgressFilter narrows the progress list. A zero WorkDate is today in each project's timezone.
type ProgressFilter struct {
	ProjectCode string
	TeamCode    string
	Type        string
	WorkDate    *timeutil.Date
}

// Progress returns the counters of the zones of the projects the caller serves in.
//
// Whose scans are counted: a member sees their own regu's; a DANRU sees their own regu's and every
// regu of their approve permission, and can pick one with TeamCode. A regu outside that is
// PERMISSION_DENIED, as is a project the caller does not serve in.
func (s *Service) Progress(ctx context.Context, p auth.Principal, f ProgressFilter) ([]Progress, error) {
	out := []Progress{}
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		mem, err := s.membershipsOf(ctx, q, p.PersonnelID)
		if err != nil {
			return err
		}
		projects := mem.projects()
		if f.ProjectCode != "" {
			if !slices.Contains(projects, f.ProjectCode) {
				return access.ErrPermissionDenied
			}
			projects = []string{f.ProjectCode}
		}
		types := []db.PatrolZoneType{db.PatrolZoneTypePATROL, db.PatrolZoneTypeGUARD}
		if f.Type != "" {
			types = []db.PatrolZoneType{db.PatrolZoneType(f.Type)}
		}
		picked := false // a regu was asked for and the caller may see it for at least one zone type
		for _, projectCode := range projects {
			project, err := q.GetProject(ctx, projectCode)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return fmt.Errorf("get project: %w", err)
			}
			loc, err := time.LoadLocation(project.Timezone)
			if err != nil {
				return fmt.Errorf("project %s: timezone %q: %w", project.Code, project.Timezone, err)
			}
			day := timeutil.BusinessDate(s.clock.Now(), loc)
			if f.WorkDate != nil {
				day = *f.WorkDate
			}
			var mine []string
			for _, m := range mem {
				if m.ProjectCode == projectCode {
					mine = append(mine, m.TeamCode)
				}
			}
			for _, t := range types {
				broad, err := s.approveReach(ctx, q, p.PersonnelID, t, day)
				if err != nil {
					return err
				}
				allowed := slices.Clone(mine)
				for _, tc := range broad {
					if access.ProjectCodeOfTeam(tc) == projectCode {
						allowed = append(allowed, tc)
					}
				}
				slices.Sort(allowed)
				allowed = slices.Compact(allowed)
				teams := allowed
				if f.TeamCode != "" {
					// The permission differs by zone type: a regu can be in reach for patrol zones and
					// not for guard zones. A type the caller cannot see this regu's counts of is left
					// out; the request is refused only when no type lets them (below).
					if !slices.Contains(allowed, f.TeamCode) {
						continue
					}
					picked = true
					teams = []string{f.TeamCode}
				}
				rows, err := q.ZoneProgress(ctx, db.ZoneProgressParams{
					WorkDate: pgtype.Date{Time: day.Time(), Valid: true}, TeamCodes: teams, ProjectCode: projectCode, Type: t,
				})
				if err != nil {
					return fmt.Errorf("zone progress: %w", err)
				}
				for _, r := range rows {
					out = append(out, Progress{
						Zone: ZoneRef{
							ID: r.ID, ProjectCode: r.ProjectCode, TeamCode: textPtr(r.TeamCode), Type: string(r.Type), Name: r.Name,
							Alias: textPtr(r.Alias), MinVisit: int(r.MinVisit),
						},
						Required: int(r.MinVisit), Submitted: int(r.Submitted), Approved: int(r.Approved), Declined: int(r.Declined),
					})
				}
			}
		}
		if f.TeamCode != "" && !picked {
			return access.ErrPermissionDenied
		}
		return nil
	})
	return out, err
}

// Scanner is one person who scanned a zone, with the number shown next to their name.
type Scanner struct {
	PersonnelID    int64
	FullName       string
	SubmittedCount int
	ApprovedCount  int
}

// Scanners returns who scanned the zone on a day (zero is today in the project's timezone), with
// their counts (§3.2.6.2). A DANRU sees everyone of the regu they decide for, or of the one they
// pick; anyone else sees only themselves.
func (s *Service) Scanners(ctx context.Context, p auth.Principal, zoneID int64, teamCode string, workDate *timeutil.Date) ([]Scanner, error) {
	out := []Scanner{}
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		zone, project, err := loadZone(ctx, q, zoneID)
		if err != nil {
			return err
		}
		sc, err := s.scopeOf(ctx, q, p, zone, project)
		if err != nil {
			return err
		}
		allowed, personnel := sc.teams, pgtype.Int8{}
		if sc.own {
			allowed, personnel = sc.mine, pgtype.Int8{Int64: p.PersonnelID, Valid: true}
		}
		teams := allowed
		if teamCode != "" {
			if !slices.Contains(allowed, teamCode) {
				return access.ErrPermissionDenied
			}
			teams = []string{teamCode}
		}
		if teams == nil {
			teams = []string{}
		}
		loc, err := time.LoadLocation(project.Timezone)
		if err != nil {
			return fmt.Errorf("project %s: timezone %q: %w", project.Code, project.Timezone, err)
		}
		day := timeutil.BusinessDate(s.clock.Now(), loc)
		if workDate != nil {
			day = *workDate
		}
		rows, err := q.ZoneScanners(ctx, db.ZoneScannersParams{
			ZoneID: zoneID, WorkDate: pgtype.Date{Time: day.Time(), Valid: true}, TeamCodes: teams, PersonnelID: personnel,
		})
		if err != nil {
			return fmt.Errorf("zone scanners: %w", err)
		}
		for _, r := range rows {
			out = append(out, Scanner{PersonnelID: r.PersonnelID, FullName: r.FullName, SubmittedCount: int(r.SubmittedCount), ApprovedCount: int(r.ApprovedCount)})
		}
		return nil
	})
	return out, err
}

// Decision is what a decision did: the scans it changed, and the ids it left alone because they
// were already decided.
type Decision struct {
	Scans   []Scan
	Skipped []int64
}

// Approve approves scans, and Decline declines them (one scan, with a note, in practice). See decide.
func (s *Service) Approve(ctx context.Context, p auth.Principal, ids []int64) (Decision, error) {
	return s.decide(ctx, p, ids, db.PatrolScanStatusAPPROVED, nil)
}

// Decline declines scans with the DANRU's note (the APK's danru_patrol_feedback).
func (s *Service) Decline(ctx context.Context, p auth.Principal, ids []int64, note string) (Decision, error) {
	note = strings.TrimSpace(note)
	return s.decide(ctx, p, ids, db.PatrolScanStatusDECLINED, &note)
}

// decide approves or declines scans in one transaction.
//
// Every scan must exist and be one the caller decides for (patrol.approve or guard.approve, by the
// zone's type, with the scan's regu in reach on the scan's work date): one that is not fails the
// whole request, so a bulk is never half applied by a permission. What is then left to the database
// is the race between two deciders: the UPDATE only touches rows that are still SUBMITTED, and the
// ids it does not return were decided already, which the caller reports (a single decision as
// SCAN_ALREADY_DECIDED, a bulk by listing them as skipped).
//
// After the commit each author is told ONCE, however many of their scans the request covers.
//
// ⚠ A DANRU may decide their own scan. That is the product's rule (unlike leave, where a person never
// approves their own request): a DANRU's round is checked by the DANRU who walked it, and nobody is
// above them in the regu. Do not add a check on the author here. TestPatrolDecisions holds the rule.
func (s *Service) decide(ctx context.Context, p auth.Principal, ids []int64, status db.PatrolScanStatus, note *string) (Decision, error) {
	ids = dedupe(ids)
	var (
		out     Decision
		notices []Notice
	)
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		rows, err := q.GetPatrolScansByIDs(ctx, ids)
		if err != nil {
			return fmt.Errorf("get scans: %w", err)
		}
		if len(rows) != len(ids) {
			return ErrScanNotFound
		}
		type key struct {
			zoneType db.PatrolZoneType
			day      timeutil.Date
		}
		reach := map[key][]string{}
		for _, r := range rows {
			k := key{r.ZoneType, dateOf(r.WorkDate)}
			teams, cached := reach[k]
			if !cached {
				if teams, err = s.approveReach(ctx, q, p.PersonnelID, r.ZoneType, k.day); err != nil {
					return err
				}
				reach[k] = teams
			}
			if !slices.Contains(teams, r.TeamCode) {
				return access.ErrPermissionDenied
			}
		}

		decided, err := q.DecideScans(ctx, db.DecideScansParams{
			Status: status, DecidedBy: pgtype.Int8{Int64: p.PersonnelID, Valid: true}, Now: ts(s.clock.Now()),
			Note: textOrNull(note), Ids: ids,
		})
		if err != nil {
			return fmt.Errorf("decide scans: %w", err)
		}
		slices.SortFunc(decided, func(a, b db.PatrolScan) int { return int(a.ID - b.ID) })
		got := make(map[int64]bool, len(decided))
		for _, r := range decided {
			got[r.ID] = true
		}
		for _, id := range ids {
			if !got[id] {
				out.Skipped = append(out.Skipped, id)
			}
		}

		action := "patrol.scan.approve"
		if status == db.PatrolScanStatusDECLINED {
			action = "patrol.scan.decline"
		}
		actor := p.PersonnelID
		for _, r := range decided {
			meta := map[string]any{"zone_id": r.ZoneID, "team_code": r.TeamCode, "author_id": r.PersonnelID}
			if note != nil && *note != "" {
				meta["note"] = *note
			}
			if err := audit.Record(ctx, q, audit.Entry{
				ActorPersonnelID: &actor, Action: action, EntityType: "patrol_scan", EntityID: r.ID, Metadata: meta,
			}); err != nil {
				return err
			}
		}

		if out.Scans, err = s.present(ctx, q, decided); err != nil {
			return err
		}
		notices, err = s.decisionNotices(ctx, q, p, decided, status == db.PatrolScanStatusDECLINED, note)
		return err
	})
	if err != nil {
		return Decision{}, err
	}
	s.notify(ctx, notices...)
	return out, nil
}

// decisionNotices makes one notice per author and zone type for the scans a request decided. The
// type is part of the key because patrol and guard scans are two kinds of notification; an author
// whose request covers both gets one of each.
func (s *Service) decisionNotices(ctx context.Context, q *db.Queries, p auth.Principal, decided []db.PatrolScan, declined bool, note *string) ([]Notice, error) {
	if s.notifier == nil || len(decided) == 0 {
		return nil, nil
	}
	decider, err := q.PersonnelNameAndNIK(ctx, p.PersonnelID)
	if err != nil {
		return nil, fmt.Errorf("get decider name: %w", err)
	}
	type key struct {
		author   int64
		zoneType db.PatrolZoneType
	}
	order := []key{}
	groups := map[key][]db.PatrolScan{}
	for _, r := range decided {
		// A decider who approves their own scan does not need telling.
		if r.PersonnelID == p.PersonnelID {
			continue
		}
		k := key{r.PersonnelID, r.ZoneType}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], r)
	}
	lines := map[string]string{}
	var out []Notice
	for _, k := range order {
		scans := groups[k]
		first := scans[0]
		line, ok := lines[first.ProjectCode]
		if !ok {
			project, err := q.GetProject(ctx, first.ProjectCode)
			if err != nil {
				return nil, fmt.Errorf("get project: %w", err)
			}
			line = string(project.BusinessLine)
			lines[first.ProjectCode] = line
		}
		subject := first.Subject
		if len(scans) > 1 {
			subject = fmt.Sprintf("%d scan", len(scans))
		}
		n := Notice{
			Kind: NoticeDecided, RecipientIDs: []int64{k.author}, ZoneType: string(k.zoneType), BusinessLine: line,
			ActorName: decider.FullName, TeamCode: first.TeamCode, Subject: subject, Date: dateOf(first.WorkDate),
			ScanID: first.ID, ZoneID: first.ZoneID, Declined: declined,
		}
		if note != nil {
			n.DecisionNote = *note
		}
		for _, r := range scans {
			n.ScanIDs = append(n.ScanIDs, r.ID)
		}
		out = append(out, n)
	}
	return out, nil
}

func dedupe(ids []int64) []int64 {
	seen := make(map[int64]bool, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func positive(n int64) *int64 {
	if n < 1 {
		return nil
	}
	return &n
}

func scanStatus(s string) db.NullPatrolScanStatus {
	return db.NullPatrolScanStatus{PatrolScanStatus: db.PatrolScanStatus(s), Valid: s != ""}
}
