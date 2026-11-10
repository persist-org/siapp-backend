package leave

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/timeutil"
)

// ClientQuota is the quota of one client.
type ClientQuota struct {
	ClientCode string `json:"client_code" validate:"required,max=40"`
	QuotaDays  int    `json:"quota_days" validate:"min=0,max=366"`
}

// Quotas is the quotas of a category: the organization's default and the clients' own.
type Quotas struct {
	// DefaultDays is nil when the organization has no default: such a client has 0 days.
	DefaultDays *int          `json:"default_days"`
	Clients     []ClientQuota `json:"clients"`
}

// QuotasInput replaces the quotas of a category as a whole.
type QuotasInput struct {
	DefaultDays *int          `json:"default_days" validate:"omitempty,min=0,max=366"`
	Clients     []ClientQuota `json:"clients" validate:"omitempty,max=500,dive"`
}

func quotasOf(ctx context.Context, q *db.Queries, categoryID int64) (Quotas, error) {
	rows, err := q.ListLeaveCategoryQuotas(ctx, categoryID)
	if err != nil {
		return Quotas{}, fmt.Errorf("list leave quotas: %w", err)
	}
	out := Quotas{Clients: []ClientQuota{}}
	for _, r := range rows {
		if !r.ClientCode.Valid {
			d := int(r.QuotaDays)
			out.DefaultDays = &d
			continue
		}
		out.Clients = append(out.Clients, ClientQuota{ClientCode: r.ClientCode.String, QuotaDays: int(r.QuotaDays)})
	}
	return out, nil
}

// SetQuotas replaces the quotas of a category: the rows sent are the rows there are afterwards.
//
// ⚠ No row means 0 days, as in kpi-personalia-api: a category without a quota cannot be requested.
// Sick leave has none, and annual leave is limited by the ledger, so both are refused here.
func (s *Service) SetQuotas(ctx context.Context, actor auth.Principal, scope access.AdminScope, code string, in QuotasInput) (Quotas, error) {
	if err := requireOrgAdmin(scope); err != nil {
		return Quotas{}, err
	}
	seen := map[string]bool{}
	for i, c := range in.Clients {
		if seen[c.ClientCode] {
			return Quotas{}, validationError(fmt.Sprintf("clients[%d].client_code", i), "Klien ini sudah ada dalam daftar.")
		}
		seen[c.ClientCode] = true
	}
	var out Quotas
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		cat, err := q.GetLeaveCategoryForUpdate(ctx, code)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCategoryNotFound
		}
		if err != nil {
			return fmt.Errorf("get leave category: %w", err)
		}
		if cat.IsAnnual || !cat.HasQuota {
			return ErrCategoryInvalid
		}
		for i, c := range in.Clients {
			ok, err := q.LeaveClientExists(ctx, c.ClientCode)
			if err != nil {
				return fmt.Errorf("client exists: %w", err)
			}
			if !ok {
				return validationError(fmt.Sprintf("clients[%d].client_code", i), "Klien tidak ditemukan.")
			}
		}
		now := ts(s.clock.Now())
		if err := q.DeleteLeaveCategoryQuotas(ctx, cat.ID); err != nil {
			return fmt.Errorf("delete leave quotas: %w", err)
		}
		if in.DefaultDays != nil {
			if err := q.InsertLeaveCategoryQuota(ctx, db.InsertLeaveCategoryQuotaParams{
				CategoryID: cat.ID, QuotaDays: int32(*in.DefaultDays), Now: now, //nolint:gosec // validated 0..366
			}); err != nil {
				return fmt.Errorf("insert default quota: %w", err)
			}
		}
		for _, c := range in.Clients {
			if err := q.InsertLeaveCategoryQuota(ctx, db.InsertLeaveCategoryQuotaParams{
				CategoryID: cat.ID, ClientCode: textOrNull(c.ClientCode), QuotaDays: int32(c.QuotaDays), Now: now, //nolint:gosec // validated 0..366
			}); err != nil {
				return fmt.Errorf("insert client quota: %w", err)
			}
		}
		if out, err = quotasOf(ctx, q, cat.ID); err != nil {
			return err
		}
		actorID := actor.PersonnelID
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "leave.quota.set", EntityType: "leave_category", EntityID: cat.ID,
			Metadata: map[string]any{"code": cat.Code, "clients": len(in.Clients), "has_default": in.DefaultDays != nil},
		})
	})
	return out, err
}

// SetClientEnabled switches a client's people on or off for requests (KPI's
// can_request_leave_permit). A client with no setting is enabled.
func (s *Service) SetClientEnabled(ctx context.Context, actor auth.Principal, scope access.AdminScope, clientCode string, enabled bool) error {
	if err := requireOrgAdmin(scope); err != nil {
		return err
	}
	return database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		ok, err := q.LeaveClientExists(ctx, clientCode)
		if err != nil {
			return fmt.Errorf("client exists: %w", err)
		}
		if !ok {
			return ErrClientNotFound
		}
		if err := q.UpsertLeaveClientSetting(ctx, db.UpsertLeaveClientSettingParams{
			ClientCode: clientCode, RequestsEnabled: enabled, Now: ts(s.clock.Now()),
		}); err != nil {
			return fmt.Errorf("upsert leave client setting: %w", err)
		}
		actorID := actor.PersonnelID
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "leave.client_setting.set", EntityType: "client", EntityCode: clientCode,
			Metadata: map[string]any{"requests_enabled": enabled},
		})
	})
}

// Quota is what a person may still take of a category, in days.
type Quota struct {
	CategoryCode string
	// QuotaDays is the limit: the ledger balance for annual leave, the client's days otherwise.
	QuotaDays int
	// UsedDays is what pending and approved requests of the calendar year already hold.
	UsedDays      int
	RemainingDays int
}

// Allowance is the result of a quota lookup inside a transaction, for Part B's submit too.
type Allowance struct {
	Category Category
	Quota
}

// memberTeam picks the regu a person asks about: the one they named, which they must serve in
// today, or their only one. A person in two regu has to say which; a person in none cannot ask.
func (s *Service) memberTeam(ctx context.Context, q *db.Queries, personnelID int64, teamCode string) (string, error) {
	rows, err := q.ListCurrentMembershipsOfPersonnel(ctx, db.ListCurrentMembershipsOfPersonnelParams{PersonnelID: personnelID, Now: ts(s.clock.Now())})
	if err != nil {
		return "", fmt.Errorf("list current memberships: %w", err)
	}
	codes := make([]string, 0, len(rows))
	for _, r := range rows {
		codes = append(codes, r.TeamCode)
	}
	switch {
	case teamCode != "":
		if !slices.Contains(codes, teamCode) {
			return "", access.ErrPermissionDenied
		}
		return teamCode, nil
	case len(codes) == 1:
		return codes[0], nil
	case len(codes) == 0:
		return "", access.ErrPermissionDenied
	default:
		return "", validationError("team_code", "Wajib diisi: Anda bertugas di lebih dari satu regu.")
	}
}

// ClientAndZone is where a regu is: its client (whose quota applies) and the timezone of its
// project (whose calendar year it is).
func clientAndZone(ctx context.Context, q *db.Queries, teamCode string) (clientCode string, loc *time.Location, err error) {
	team, err := q.GetTeam(ctx, teamCode)
	if err != nil {
		return "", nil, fmt.Errorf("get team %s: %w", teamCode, err)
	}
	project, err := q.GetProject(ctx, team.ProjectCode)
	if err != nil {
		return "", nil, fmt.Errorf("get project %s: %w", team.ProjectCode, err)
	}
	loc, err = time.LoadLocation(project.Timezone)
	if err != nil {
		return "", nil, fmt.Errorf("load location %s: %w", project.Timezone, err)
	}
	return project.ClientCode, loc, nil
}

// AllowanceFor computes what the person may still take of the category, for a request in the
// given regu on the given day, inside the caller's transaction. Part B's submit calls it under the
// person's lock.
//
// Annual leave: the limit is the ledger balance (pending days are already deducted from it).
// Everything else: the days of the client's quota (else the organization's default, else 0) less
// the days pending and approved requests hold in the calendar year of the day, in the project's
// timezone. A client switched off has 0.
//
// ⚠ kpi-personalia-api counts REQUESTS for that, compares the count with the days of a request and
// ignores pending ones; SiAPP counts days, pending included (docs/plan/07-attendance.md, deviation
// 3). Sick leave and categories without a quota have none: ErrCategoryInvalid.
func (s *Service) AllowanceFor(ctx context.Context, q *db.Queries, personnelID int64, categoryCode, teamCode string, on timeutil.Date) (Allowance, error) {
	row, err := categoryByCode(ctx, q, categoryCode)
	if err != nil {
		return Allowance{}, err
	}
	cat := toCategory(row)
	if !cat.Active || !cat.HasQuota {
		return Allowance{}, ErrCategoryInvalid
	}
	out := Allowance{Category: cat, Quota: Quota{CategoryCode: cat.Code}}

	clientCode, loc, err := clientAndZone(ctx, q, teamCode)
	if err != nil {
		return Allowance{}, err
	}
	if cat.IsAnnual {
		balance, err := q.GetLeaveBalance(ctx, personnelID)
		if err != nil {
			return Allowance{}, fmt.Errorf("get leave balance: %w", err)
		}
		out.QuotaDays, out.RemainingDays = int(balance), int(balance)
		return out, s.addUsed(ctx, q, personnelID, cat, on, loc, &out.UsedDays)
	}

	enabled, err := q.GetLeaveClientSetting(ctx, clientCode)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Allowance{}, fmt.Errorf("get leave client setting: %w", err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		enabled = true
	}
	if enabled {
		days, err := q.GetEffectiveLeaveQuota(ctx, db.GetEffectiveLeaveQuotaParams{CategoryID: cat.ID, ClientCode: textOrNull(clientCode)})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Allowance{}, fmt.Errorf("get leave quota: %w", err)
		}
		out.QuotaDays = int(days) // no row is 0 days
	}
	if err := s.addUsed(ctx, q, personnelID, cat, on, loc, &out.UsedDays); err != nil {
		return Allowance{}, err
	}
	out.RemainingDays = max(0, out.QuotaDays-out.UsedDays)
	return out, nil
}

// addUsed fills in the days a person's live requests of a category already hold. A day count of
// annual leave is the days not yet taken (their request ends today or later): the days already
// taken are not "used" any more, they are gone from the balance.
func (s *Service) addUsed(ctx context.Context, q *db.Queries, personnelID int64, cat Category, on timeutil.Date, loc *time.Location, into *int) error {
	year := on.Time().Year()
	notBefore := timeutil.NewDate(year, time.January, 1)
	if cat.IsAnnual {
		notBefore = timeutil.BusinessDate(s.clock.Now(), loc)
	}
	days, err := q.SumLiveLeaveDays(ctx, db.SumLiveLeaveDaysParams{
		PersonnelID: personnelID, CategoryID: cat.ID,
		YearStart: dateParam(timeutil.NewDate(year, time.January, 1)), YearEnd: dateParam(timeutil.NewDate(year, time.December, 31)),
		NotBefore: dateParam(notBefore),
	})
	if err != nil {
		return fmt.Errorf("sum live leave days: %w", err)
	}
	*into = int(days)
	return nil
}

// QuotaOf is GET /permissions/quota: the caller's allowance for a category in a regu they serve
// in (the only one when teamCode is empty), as of today in the project's timezone.
func (s *Service) QuotaOf(ctx context.Context, p auth.Principal, categoryCode, teamCode string) (Quota, error) {
	var out Quota
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		team, err := s.memberTeam(ctx, q, p.PersonnelID, teamCode)
		if err != nil {
			return err
		}
		_, loc, err := clientAndZone(ctx, q, team)
		if err != nil {
			return err
		}
		a, err := s.AllowanceFor(ctx, q, p.PersonnelID, categoryCode, team, timeutil.BusinessDate(s.clock.Now(), loc))
		out = a.Quota
		return err
	})
	return out, err
}

// Approver is who will decide the caller's request in a regu: its DANRU today.
type Approver struct {
	PersonnelID int64
	Name        string
}

// ApproverOf is GET /permissions/approver. A regu with no DANRU today, or whose DANRU is the
// caller, has none: ErrNoApprover (the request still works, the admins decide it).
func (s *Service) ApproverOf(ctx context.Context, p auth.Principal, teamCode string) (Approver, error) {
	var out Approver
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		team, err := s.memberTeam(ctx, q, p.PersonnelID, teamCode)
		if err != nil {
			return err
		}
		_, loc, err := clientAndZone(ctx, q, team)
		if err != nil {
			return err
		}
		today := timeutil.BusinessDate(s.clock.Now(), loc)
		d, err := q.GetTeamDanruOn(ctx, db.GetTeamDanruOnParams{TeamCode: team, OnDate: dateParam(today)})
		if errors.Is(err, pgx.ErrNoRows) || err == nil && d.PersonnelID == p.PersonnelID {
			return ErrNoApprover
		}
		if err != nil {
			return fmt.Errorf("get danru: %w", err)
		}
		out = Approver{PersonnelID: d.PersonnelID, Name: d.FullName}
		return nil
	})
	return out, err
}
