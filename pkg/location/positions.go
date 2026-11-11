package location

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"siapp/internal/db"
	"siapp/pkg/platform/database"
)

// Position is a person on the map: who they are, whether they are online and where they were last
// seen. The coordinates are nil when the person never sent a point, or is not online (Online
// lists only show online people, so there they are always set).
type Position struct {
	PersonnelID  int64
	FullName     string
	PhotoURL     *string
	Role         string
	PositionName *string
	TeamCode     string
	ProjectCode  string
	Online       bool
	LastSeenAt   *time.Time

	Latitude       *float64
	Longitude      *float64
	AccuracyMeters *float64
	LoggedAt       *time.Time
}

// Filter narrows the people on a map. A nil slice means "no restriction"; an empty, non-nil one
// reaches nobody. Callers fill in what the caller's permission reaches, so a filter never widens
// it.
type Filter struct {
	TeamCodes    []string
	ProjectCodes []string
}

// OnMap returns the online members of the regu in the filter, with their last location. A person
// who is offline, or who never sent a point, is not on the map: the position of someone who has
// closed the app says nothing about where they are now.
func (s *Service) OnMap(ctx context.Context, orgID int64, f Filter) ([]Position, error) {
	if (f.TeamCodes != nil && len(f.TeamCodes) == 0) || (f.ProjectCodes != nil && len(f.ProjectCodes) == 0) {
		return []Position{}, nil
	}
	var out []Position
	err := database.WithTenant(ctx, s.pool, orgID, func(q *db.Queries) error {
		all, err := s.positions(ctx, q, orgID, f, 0)
		if err != nil {
			return err
		}
		out = make([]Position, 0, len(all))
		for _, p := range all {
			if p.Online && p.Latitude != nil {
				out = append(out, p)
			}
		}
		return nil
	})
	return out, err
}

// ErrNotInScope is returned by Of when the person is not a member of any regu in the caller's
// reach (or does not exist: the two are not told apart).
var ErrNotInScope = errors.New("location: person is not in scope")

// Of returns one person's position, for a caller whose reach is teamCodes. A member who is in
// scope but offline is returned with Online false and no coordinates, so the app can say so
// instead of showing a stale pin.
func (s *Service) Of(ctx context.Context, orgID int64, teamCodes []string, personnelID int64) (Position, error) {
	var out Position
	if len(teamCodes) == 0 {
		return out, ErrNotInScope
	}
	err := database.WithTenant(ctx, s.pool, orgID, func(q *db.Queries) error {
		all, err := s.positions(ctx, q, orgID, Filter{TeamCodes: teamCodes}, personnelID)
		if err != nil {
			return err
		}
		if len(all) == 0 {
			return ErrNotInScope
		}
		out = all[0]
		if !out.Online {
			out.Latitude, out.Longitude, out.AccuracyMeters, out.LoggedAt = nil, nil, nil, nil
		}
		return nil
	})
	return out, err
}

// positions reads the members in f (or one person), each once, with their presence. A person who
// serves in two of the regu is listed under the first, in the query's order.
func (s *Service) positions(ctx context.Context, q *db.Queries, orgID int64, f Filter, personnelID int64) ([]Position, error) {
	params := db.ListMemberLocationsParams{Now: pgtype.Timestamptz{Time: s.clock.Now(), Valid: true}, TeamCodes: f.TeamCodes, ProjectCodes: f.ProjectCodes}
	if personnelID != 0 {
		params.PersonnelID = pgtype.Int8{Int64: personnelID, Valid: true}
	}
	rows, err := q.ListMemberLocations(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("location: list member locations: %w", err)
	}
	ids := make([]int64, 0, len(rows))
	seen := make(map[int64]bool, len(rows))
	for _, r := range rows {
		if !seen[r.PersonnelID] {
			seen[r.PersonnelID] = true
			ids = append(ids, r.PersonnelID)
		}
	}
	status, err := s.presence.ManyIn(ctx, q, orgID, ids)
	if err != nil {
		return nil, fmt.Errorf("location: presence of members: %w", err)
	}
	out := make([]Position, 0, len(ids))
	listed := make(map[int64]bool, len(ids))
	for _, r := range rows {
		if listed[r.PersonnelID] {
			continue
		}
		listed[r.PersonnelID] = true
		st := status[r.PersonnelID]
		p := Position{
			PersonnelID: r.PersonnelID, FullName: r.FullName, PhotoURL: textPtr(r.PhotoUrl), Role: string(r.Role),
			PositionName: textPtr(r.PositionName), TeamCode: r.TeamCode, ProjectCode: r.ProjectCode,
			Online: st.Online, LastSeenAt: st.LastSeenAt,
			Latitude: r.Latitude, Longitude: r.Longitude, AccuracyMeters: r.AccuracyMeters,
		}
		if r.LoggedAt.Valid {
			t := r.LoggedAt.Time.UTC()
			p.LoggedAt = &t
		}
		out = append(out, p)
	}
	return out, nil
}

func textPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}
